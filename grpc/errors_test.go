package grpc

import (
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/grpc-ecosystem/go-grpc-prometheus"
	berrors "github.com/letsencrypt/boulder/errors"
	testproto "github.com/letsencrypt/boulder/grpc/test_proto"
	"github.com/letsencrypt/boulder/test"
)

type errorServer struct {
	err error
}

func (s *errorServer) Chill(_ context.Context, _ *testproto.Time) (*testproto.Time, error) {
	return nil, s.err
}

func TestErrorWrapping(t *testing.T) {
	si := serverInterceptor{grpc_prometheus.NewServerMetrics()}
	ci := clientInterceptor{time.Second, grpc_prometheus.NewClientMetrics()}
	srv := grpc.NewServer(grpc.UnaryInterceptor(si.intercept))
	es := &errorServer{}
	testproto.RegisterChillerServer(srv, es)
	lis, err := net.Listen("tcp", "127.0.0.1:")
	test.AssertNotError(t, err, "Failed to create listener")
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.Dial(
		lis.Addr().String(),
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(ci.intercept),
	)
	test.AssertNotError(t, err, "Failed to dial grpc test server")
	client := testproto.NewChillerClient(conn)

	es.err = berrors.MalformedError("yup")
	_, err = client.Chill(context.Background(), &testproto.Time{})
	test.Assert(t, err != nil, fmt.Sprintf("nil error returned, expected: %s", err))
	test.AssertDeepEquals(t, err, es.err)

	test.AssertEquals(t, wrapError(nil, nil), nil)
	test.AssertEquals(t, unwrapError(nil, nil), nil)
}
