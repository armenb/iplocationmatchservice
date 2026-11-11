package grpcapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	iplocationmatchv1 "github.com/armenb/iplocationmatch/gen/iplocationmatch/v1"
	"github.com/armenb/iplocationmatch/internal/grpcapi"
	"github.com/armenb/iplocationmatch/internal/iplocationmatch"
	"github.com/armenb/iplocationmatch/internal/testdb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// newClient serves the real handler over an in-memory listener.
func newClient(t *testing.T) iplocationmatchv1.IPLocationMatchServiceClient {
	t.Helper()
	c, err := iplocationmatch.Open(testdb.Write(t, testdb.Fixture))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	iplocationmatchv1.RegisterIPLocationMatchServiceServer(srv, grpcapi.NewServer(c, slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return iplocationmatchv1.NewIPLocationMatchServiceClient(conn)
}

func TestCheckIP(t *testing.T) {
	client := newClient(t)

	tests := []struct {
		name      string
		ip        string
		countries []string
		want      bool
		wantCode  codes.Code
	}{
		{"match", "81.2.69.142", []string{"GB", "US"}, true, codes.OK},
		{"no match", "81.2.69.142", []string{"US"}, false, codes.OK},
		{"ip not in database", "8.8.8.8", []string{"US"}, false, codes.OK},
		{"ipv6 lowercase code", "2001:218::1", []string{"jp"}, true, codes.OK},
		{"invalid ip", "nope", []string{"US"}, false, codes.InvalidArgument},
		{"invalid country", "81.2.69.142", []string{"USA"}, false, codes.InvalidArgument},
		{"no countries", "81.2.69.142", nil, false, codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: tt.ip, AllowedCountries: tt.countries})
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("code = %v, want %v (err %v)", got, tt.wantCode, err)
			}
			if err == nil && resp.GetMatch() != tt.want {
				t.Fatalf("match = %v, want %v", resp.GetMatch(), tt.want)
			}
		})
	}
}
