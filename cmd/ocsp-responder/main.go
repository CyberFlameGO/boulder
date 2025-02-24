package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha1"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-gorp/gorp/v3"
	"github.com/honeycombio/beeline-go"
	"github.com/honeycombio/beeline-go/wrappers/hnynethttp"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/ocsp"

	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/db"
	"github.com/letsencrypt/boulder/features"
	"github.com/letsencrypt/boulder/issuance"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics/measured_http"
	bocsp "github.com/letsencrypt/boulder/ocsp"
	"github.com/letsencrypt/boulder/sa"
)

// ocspFilter stores information needed to filter OCSP requests (to ensure we
// aren't trying to serve OCSP for certs which aren't ours), and surfaces
// methods to determine if a given request should be filtered or not.
type ocspFilter struct {
	issuerKeyHashAlgorithm crypto.Hash
	issuerKeyHashes        map[issuance.IssuerID][]byte
	serialPrefixes         []string
}

// newFilter creates a new ocspFilter which will accept a request only if it
// uses the SHA1 algorithm to hash the issuer key, the issuer key matches one
// of the given issuer certs (here, paths to PEM certs on disk), and the serial
// has one of the given prefixes.
func newFilter(issuerCerts []string, serialPrefixes []string) (*ocspFilter, error) {
	if len(issuerCerts) < 1 {
		return nil, errors.New("Filter must include at least 1 issuer cert")
	}
	issuerKeyHashes := make(map[issuance.IssuerID][]byte, 0)
	for _, issuerCert := range issuerCerts {
		// Load the certificate from the file path.
		cert, err := core.LoadCert(issuerCert)
		if err != nil {
			return nil, fmt.Errorf("Could not load issuer cert %s: %w", issuerCert, err)
		}
		caCert := &issuance.Certificate{Certificate: cert}
		// The issuerKeyHash in OCSP requests is constructed over the DER
		// encoding of the public key per RFC 6960 (defined in RFC 4055 for
		// RSA and RFC 5480 for ECDSA). We can't use MarshalPKIXPublicKey
		// for this since it encodes keys using the SPKI structure itself,
		// and we just want the contents of the subjectPublicKey for the
		// hash, so we need to extract it ourselves.
		var spki struct {
			Algo      pkix.AlgorithmIdentifier
			BitString asn1.BitString
		}
		if _, err := asn1.Unmarshal(caCert.RawSubjectPublicKeyInfo, &spki); err != nil {
			return nil, err
		}
		keyHash := sha1.Sum(spki.BitString.Bytes)
		issuerKeyHashes[caCert.ID()] = keyHash[:]
	}
	return &ocspFilter{crypto.SHA1, issuerKeyHashes, serialPrefixes}, nil
}

// checkRequest returns a descriptive error if the request does not satisfy any of
// the requirements of an OCSP request, or nil if the request should be handled.
func (f *ocspFilter) checkRequest(req *ocsp.Request) error {
	if req.HashAlgorithm != f.issuerKeyHashAlgorithm {
		return fmt.Errorf("Request ca key hash using unsupported algorithm %s: %w", req.HashAlgorithm, bocsp.ErrNotFound)
	}
	// Check that this request is for the proper CA
	match := false
	for _, keyHash := range f.issuerKeyHashes {
		if match = bytes.Equal(req.IssuerKeyHash, keyHash); match {
			break
		}
	}
	if !match {
		return fmt.Errorf("Request intended for wrong issuer cert %s: %w", hex.EncodeToString(req.IssuerKeyHash), bocsp.ErrNotFound)
	}

	serialString := core.SerialToString(req.SerialNumber)
	if len(f.serialPrefixes) > 0 {
		match := false
		for _, prefix := range f.serialPrefixes {
			if match = strings.HasPrefix(serialString, prefix); match {
				break
			}
		}
		if !match {
			return fmt.Errorf("Request serial has wrong prefix: %w", bocsp.ErrNotFound)
		}
	}

	return nil
}

// responseMatchesIssuer returns true if the CertificateStatus (from the db)
// was generated by an issuer matching the key hash in the original request.
// This filters out, for example, responses which are for a serial that we
// issued, but from a different issuer than that contained in the request.
func (f *ocspFilter) responseMatchesIssuer(req *ocsp.Request, status core.CertificateStatus) bool {
	issuerKeyHash, ok := f.issuerKeyHashes[issuance.IssuerID(*status.IssuerID)]
	if !ok {
		return false
	}
	return bytes.Equal(issuerKeyHash, req.IssuerKeyHash)
}

// dbSource represents a database containing pre-generated OCSP responses keyed
// by serial number. It also allows for filtering requests by their issuer key
// hash and serial number, to prevent unnecessary lookups for rows that we know
// will not exist in the database.
//
// We assume that OCSP responses are stored in a very simple database table,
// with at least these two columns: serialNumber (TEXT) and response (BLOB).
//
// The serialNumber field may have any type to which Go will match a string,
// so you can be more efficient than TEXT if you like. We use it to store the
// serial number in hex. You must have an index on the serialNumber field,
// since we will always query on it.
type dbSource struct {
	dbMap   dbSelector
	filter  *ocspFilter
	timeout time.Duration
	log     blog.Logger
}

// Define an interface with the needed methods from gorp.
// This also allows us to simulate MySQL failures by mocking the interface.
type dbSelector interface {
	SelectOne(holder interface{}, query string, args ...interface{}) error
	WithContext(ctx context.Context) gorp.SqlExecutor
}

// Response is called by the HTTP server to handle a new OCSP request.
func (src *dbSource) Response(req *ocsp.Request) ([]byte, http.Header, error) {
	err := src.filter.checkRequest(req)
	if err != nil {
		src.log.Debugf("Not responding to filtered OCSP request: %s", err.Error())
		return nil, nil, err
	}

	serialString := core.SerialToString(req.SerialNumber)
	src.log.Debugf("Searching for OCSP issued by us for serial %s", serialString)

	var certStatus core.CertificateStatus
	defer func() {
		if len(certStatus.OCSPResponse) != 0 {
			src.log.Debugf("OCSP Response sent for CA=%s, Serial=%s", hex.EncodeToString(req.IssuerKeyHash), serialString)
		}
	}()
	ctx := context.Background()
	if src.timeout != 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, src.timeout)
		defer cancel()
	}
	certStatus, err = sa.SelectCertificateStatus(src.dbMap.WithContext(ctx), serialString)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, nil, bocsp.ErrNotFound
		}
		src.log.AuditErrf("Looking up OCSP response: %s", err)
		return nil, nil, err
	}
	if certStatus.IsExpired {
		src.log.Infof("OCSP Response not sent (expired) for CA=%s, Serial=%s", hex.EncodeToString(req.IssuerKeyHash), serialString)
		return nil, nil, bocsp.ErrNotFound
	} else if certStatus.OCSPLastUpdated.IsZero() {
		src.log.Warningf("OCSP Response not sent (ocspLastUpdated is zero) for CA=%s, Serial=%s", hex.EncodeToString(req.IssuerKeyHash), serialString)
		return nil, nil, bocsp.ErrNotFound
	} else if !src.filter.responseMatchesIssuer(req, certStatus) {
		src.log.Warningf("OCSP Response not sent (issuer and serial mismatch) for CA=%s, Serial=%s", hex.EncodeToString(req.IssuerKeyHash), serialString)
		return nil, nil, bocsp.ErrNotFound
	}
	return certStatus.OCSPResponse, nil, nil
}

type config struct {
	OCSPResponder struct {
		cmd.ServiceConfig
		DB cmd.DBConfig

		// Source indicates the source of pre-signed OCSP responses to be used. It
		// can be a DBConnect string or a file URL. The file URL style is used
		// when responding from a static file for intermediates and roots.
		// If DBConfig has non-empty fields, it takes precedence over this.
		Source string

		// The list of issuer certificates, against which OCSP requests/responses
		// are checked to ensure we're not responding for anyone else's certs.
		IssuerCerts []string

		Path          string
		ListenAddress string
		// MaxAge is the max-age to set in the Cache-Control response
		// header. It is a time.Duration formatted string.
		MaxAge cmd.ConfigDuration

		// When to timeout a request. This should be slightly lower than the
		// upstream's timeout when making request to ocsp-responder.
		Timeout cmd.ConfigDuration

		ShutdownStopTimeout cmd.ConfigDuration

		RequiredSerialPrefixes []string

		Features map[string]bool
	}

	Syslog  cmd.SyslogConfig
	Beeline cmd.BeelineConfig
}

func main() {
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	flag.Parse()
	if *configFile == "" {
		fmt.Fprintf(os.Stderr, `Usage of %s:
Config JSON should contain either a DBConnectFile or a Source value containing a file: URL.
If Source is a file: URL, the file should contain a list of OCSP responses in base64-encoded DER,
as generated by Boulder's ceremony command.
`, os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	var c config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")
	err = features.Set(c.OCSPResponder.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	bc, err := c.Beeline.Load()
	cmd.FailOnError(err, "Failed to load Beeline config")
	beeline.Init(bc)
	defer beeline.Close()

	stats, logger := cmd.StatsAndLogging(c.Syslog, c.OCSPResponder.DebugAddr)
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString())

	config := c.OCSPResponder
	var source bocsp.Source

	if strings.HasPrefix(config.Source, "file:") {
		url, err := url.Parse(config.Source)
		cmd.FailOnError(err, "Source was not a URL")
		filename := url.Path
		// Go interprets cwd-relative file urls (file:test/foo.txt) as having the
		// relative part of the path in the 'Opaque' field.
		if filename == "" {
			filename = url.Opaque
		}
		source, err = bocsp.NewMemorySourceFromFile(filename, logger)
		cmd.FailOnError(err, fmt.Sprintf("Couldn't read file: %s", url.Path))
	} else {
		// For databases, DBConfig takes precedence over Source, if present.
		dbConnect, err := config.DB.URL()
		cmd.FailOnError(err, "Reading DB config")
		if dbConnect == "" {
			dbConnect = config.Source
		}
		dbSettings := sa.DbSettings{
			MaxOpenConns:    config.DB.MaxOpenConns,
			MaxIdleConns:    config.DB.MaxIdleConns,
			ConnMaxLifetime: config.DB.ConnMaxLifetime.Duration,
			ConnMaxIdleTime: config.DB.ConnMaxIdleTime.Duration,
		}
		dbMap, err := sa.NewDbMap(dbConnect, dbSettings)
		cmd.FailOnError(err, "Could not connect to database")
		sa.SetSQLDebug(dbMap, logger)

		dbAddr, dbUser, err := config.DB.DSNAddressAndUser()
		cmd.FailOnError(err, "Could not determine address or user of DB DSN")

		sa.InitDBMetrics(dbMap, stats, dbSettings, dbAddr, dbUser)

		issuerCerts := c.OCSPResponder.IssuerCerts

		filter, err := newFilter(issuerCerts, c.OCSPResponder.RequiredSerialPrefixes)
		cmd.FailOnError(err, "Couldn't create OCSP filter")

		source = &dbSource{dbMap, filter, c.OCSPResponder.Timeout.Duration, logger}

		// Export the value for dbSettings.MaxOpenConns
		dbConnStat := prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "max_db_connections",
			Help: "Maximum number of DB connections allowed.",
		})
		stats.MustRegister(dbConnStat)
		dbConnStat.Set(float64(dbSettings.MaxOpenConns))
	}

	m := mux(stats, c.OCSPResponder.Path, source, logger)
	srv := &http.Server{
		Addr:    c.OCSPResponder.ListenAddress,
		Handler: m,
	}

	done := make(chan bool)
	go cmd.CatchSignals(logger, func() {
		ctx, cancel := context.WithTimeout(context.Background(),
			c.OCSPResponder.ShutdownStopTimeout.Duration)
		defer cancel()
		_ = srv.Shutdown(ctx)
		done <- true
	})

	err = srv.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		cmd.FailOnError(err, "Running HTTP server")
	}

	// https://godoc.org/net/http#Server.Shutdown:
	// When Shutdown is called, Serve, ListenAndServe, and ListenAndServeTLS
	// immediately return ErrServerClosed. Make sure the program doesn't exit and
	// waits instead for Shutdown to return.
	<-done
}

// ocspMux partially implements the interface defined for http.ServeMux but doesn't implement
// the path cleaning its Handler method does. Notably http.ServeMux will collapse repeated
// slashes into a single slash which breaks the base64 encoding that is used in OCSP GET
// requests. ocsp.Responder explicitly recommends against using http.ServeMux
// for this reason.
type ocspMux struct {
	handler http.Handler
}

func (om *ocspMux) Handler(_ *http.Request) (http.Handler, string) {
	return om.handler, "/"
}

func mux(stats prometheus.Registerer, responderPath string, source bocsp.Source, logger blog.Logger) http.Handler {
	stripPrefix := http.StripPrefix(responderPath, bocsp.NewResponder(source, stats, logger))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "max-age=43200") // Cache for 12 hours
			w.WriteHeader(200)
			return
		}
		stripPrefix.ServeHTTP(w, r)
	})
	return hnynethttp.WrapHandler(measured_http.New(&ocspMux{h}, cmd.Clock(), stats))
}
