package server

import (
	"errors"
	"flag"
	"net/http"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	google_protobuf "github.com/golang/protobuf/ptypes/empty"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/logging"
	"github.com/weaveworks/common/middleware"
	"golang.org/x/net/context"
)

type FakeServer struct{}

func (f FakeServer) FailWithError(ctx context.Context, req *google_protobuf.Empty) (*google_protobuf.Empty, error) {
	return nil, errors.New("test error")
}

func (f FakeServer) FailWithHTTPError(ctx context.Context, req *FailWithHTTPErrorRequest) (*google_protobuf.Empty, error) {
	return nil, httpgrpc.Errorf(int(req.Code), strconv.Itoa(int(req.Code)))
}

func (f FakeServer) Succeed(ctx context.Context, req *google_protobuf.Empty) (*google_protobuf.Empty, error) {
	return &google_protobuf.Empty{}, nil
}

func (f FakeServer) Sleep(ctx context.Context, req *google_protobuf.Empty) (*google_protobuf.Empty, error) {
	err := cancelableSleep(ctx, 10*time.Second)
	return &google_protobuf.Empty{}, err
}

func (f FakeServer) StreamSleep(req *google_protobuf.Empty, stream FakeServer_StreamSleepServer) error {
	for x := 0; x < 100; x++ {
		time.Sleep(time.Second / 100.0)
		if err := stream.Send(&google_protobuf.Empty{}); err != nil {
			return err
		}
	}
	return nil
}

func cancelableSleep(ctx context.Context, sleep time.Duration) error {
	select {
	case <-time.After(sleep):
	case <-ctx.Done():
	}
	return ctx.Err()
}

// Ensure that http and grpc servers work with no overrides to config
// (except http port because an ordinary user can't bind to default port 80)
func TestDefaultAddresses(t *testing.T) {
	var cfg Config
	cfg.RegisterFlags(flag.NewFlagSet("", flag.ExitOnError))
	cfg.HTTPListenPort = 9090
	cfg.MetricsNamespace = "testing_addresses"

	server, err := New(cfg)
	require.NoError(t, err)

	fakeServer := FakeServer{}
	RegisterFakeServerServer(server.GRPC, fakeServer)

	server.HTTP.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})

	go server.Run()
	defer server.Shutdown()

	conn, err := grpc.Dial("localhost:9095", grpc.WithInsecure())
	defer conn.Close()
	require.NoError(t, err)

	empty := google_protobuf.Empty{}
	client := NewFakeServerClient(conn)
	_, err = client.Succeed(context.Background(), &empty)
	require.NoError(t, err)

	req, err := http.NewRequest("GET", "http://127.0.0.1:9090/test", nil)
	require.NoError(t, err)
	_, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
}

func TestErrorInstrumentationMiddleware(t *testing.T) {
	var cfg Config
	cfg.RegisterFlags(flag.NewFlagSet("", flag.ExitOnError))
	cfg.HTTPListenPort = 9090 // can't use 80 as ordinary user
	cfg.GRPCListenAddress = "localhost"
	cfg.GRPCListenPort = 1234
	server, err := New(cfg)
	require.NoError(t, err)

	fakeServer := FakeServer{}
	RegisterFakeServerServer(server.GRPC, fakeServer)

	server.HTTP.HandleFunc("/succeed", func(w http.ResponseWriter, r *http.Request) {
	})
	server.HTTP.HandleFunc("/error500", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	server.HTTP.HandleFunc("/sleep10", func(w http.ResponseWriter, r *http.Request) {
		_ = cancelableSleep(r.Context(), time.Second*10)
	})

	go server.Run()

	conn, err := grpc.Dial("localhost:1234", grpc.WithInsecure())
	defer conn.Close()
	require.NoError(t, err)

	empty := google_protobuf.Empty{}
	client := NewFakeServerClient(conn)
	res, err := client.Succeed(context.Background(), &empty)
	require.NoError(t, err)
	require.EqualValues(t, &empty, res)

	res, err = client.FailWithError(context.Background(), &empty)
	require.Nil(t, res)
	require.Error(t, err)

	s, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, "test error", s.Message())

	res, err = client.FailWithHTTPError(context.Background(), &FailWithHTTPErrorRequest{Code: http.StatusPaymentRequired})
	require.Nil(t, res)
	errResp, ok := httpgrpc.HTTPResponseFromError(err)
	require.True(t, ok)
	require.Equal(t, int32(http.StatusPaymentRequired), errResp.Code)
	require.Equal(t, "402", string(errResp.Body))

	callThenCancel := func(f func(ctx context.Context) error) error {
		ctx, cancel := context.WithCancel(context.Background())
		errChan := make(chan error, 1)
		go func() {
			errChan <- f(ctx)
		}()
		time.Sleep(50 * time.Millisecond) // allow the call to reach the handler
		cancel()
		return <-errChan
	}

	err = callThenCancel(func(ctx context.Context) error {
		_, err = client.Sleep(ctx, &empty)
		return err
	})
	require.Error(t, err, context.Canceled)

	err = callThenCancel(func(ctx context.Context) error {
		_, err = client.StreamSleep(ctx, &empty)
		return err
	})
	require.NoError(t, err) // canceling a streaming fn doesn't generate an error

	// Now test the HTTP versions of the functions
	{
		req, err := http.NewRequest("GET", "http://127.0.0.1:9090/succeed", nil)
		require.NoError(t, err)
		_, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
	}
	{
		req, err := http.NewRequest("GET", "http://127.0.0.1:9090/error500", nil)
		require.NoError(t, err)
		_, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
	}
	{
		req, err := http.NewRequest("GET", "http://127.0.0.1:9090/sleep10", nil)
		require.NoError(t, err)
		err = callThenCancel(func(ctx context.Context) error {
			_, err = http.DefaultClient.Do(req.WithContext(ctx))
			return err
		})
		require.Error(t, err, context.Canceled)
	}

	conn.Close()
	server.Shutdown()

	metrics, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	statuses := map[string]string{}
	for _, family := range metrics {
		if *family.Name == "request_duration_seconds" {
			for _, metric := range family.Metric {
				var route, statusCode string
				for _, label := range metric.GetLabel() {
					switch label.GetName() {
					case "status_code":
						statusCode = label.GetValue()
					case "route":
						route = label.GetValue()
					}
				}
				statuses[route] = statusCode
			}
		}
	}
	require.Equal(t, map[string]string{
		"/server.FakeServer/FailWithError":     "error",
		"/server.FakeServer/FailWithHTTPError": "402",
		"/server.FakeServer/Sleep":             "cancel",
		"/server.FakeServer/StreamSleep":       "cancel",
		"/server.FakeServer/Succeed":           "success",
		"error500":                             "500",
		"sleep10":                              "200",
		"succeed":                              "200",
	}, statuses)
}

func TestRunReturnsError(t *testing.T) {
	cfg := Config{
		HTTPListenAddress: "localhost",
		HTTPListenPort:    9190,
		GRPCListenAddress: "localhost",
		GRPCListenPort:    9191,
	}
	t.Run("http", func(t *testing.T) {
		cfg.MetricsNamespace = "testing_http"
		srv, err := New(cfg)
		require.NoError(t, err)

		errChan := make(chan error, 1)
		go func() {
			errChan <- srv.Run()
		}()

		require.NoError(t, srv.httpListener.Close())
		require.NotNil(t, <-errChan)

		// So that address is freed for further tests.
		srv.GRPC.Stop()
	})

	t.Run("grpc", func(t *testing.T) {
		cfg.MetricsNamespace = "testing_grpc"
		srv, err := New(cfg)
		require.NoError(t, err)

		errChan := make(chan error, 1)
		go func() {
			errChan <- srv.Run()
		}()

		require.NoError(t, srv.grpcListener.Close())
		require.NotNil(t, <-errChan)
	})
}

// Test to see what the logging of a 500 error looks like
func TestMiddlewareLogging(t *testing.T) {
	var level logging.Level
	level.Set("info")
	cfg := Config{
		HTTPListenAddress: "localhost",
		HTTPListenPort:    9192,
		GRPCListenAddress: "localhost",
		HTTPMiddleware:    []middleware.Interface{middleware.Logging},
		MetricsNamespace:  "testing_logging",
		LogLevel:          level,
	}
	server, err := New(cfg)
	require.NoError(t, err)

	server.HTTP.HandleFunc("/error500", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})

	go server.Run()
	defer server.Shutdown()

	req, err := http.NewRequest("GET", "http://127.0.0.1:9192/error500", nil)
	require.NoError(t, err)
	http.DefaultClient.Do(req)
}
