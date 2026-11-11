package grpcapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	iplocationmatchv1 "iplocationmatchservice/gen/iplocationmatch/v1"
	"iplocationmatchservice/internal/grpcapi"
	"iplocationmatchservice/internal/iplocationmatch"
	"iplocationmatchservice/internal/telemetry"
	"iplocationmatchservice/internal/testdb"
)

// newClient serves the real handler over an in-memory listener and returns a
// client plus a buffer receiving the server's JSON request log.
func newClient(t *testing.T) (iplocationmatchv1.IPLocationMatchServiceClient, *bytes.Buffer) {
	t.Helper()
	c, err := iplocationmatch.Open(testdb.Write(t, testdb.Fixture))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return serve(t, c)
}

func serve(t *testing.T, m grpcapi.Locator) (iplocationmatchv1.IPLocationMatchServiceClient, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	obs := telemetry.NewObserver(slog.New(slog.NewJSONHandler(&logs, nil)), nil, "test-db")

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	iplocationmatchv1.RegisterIPLocationMatchServiceServer(srv, grpcapi.NewServer(m, obs, iplocationmatch.DefaultMaxCountries))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return iplocationmatchv1.NewIPLocationMatchServiceClient(conn), &logs
}

func TestCheckIP(t *testing.T) {
	client, _ := newClient(t)

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
		{"padded ip", " 81.2.69.142 ", []string{"GB"}, false, codes.InvalidArgument},
		{"invalid country", "81.2.69.142", []string{"USA"}, false, codes.InvalidArgument},
		{"no countries", "81.2.69.142", nil, false, codes.InvalidArgument},
		{"exactly at the limit", "81.2.69.142", slices.Repeat([]string{"GB"}, iplocationmatch.DefaultMaxCountries), true, codes.OK},
		{"over the limit", "81.2.69.142", slices.Repeat([]string{"GB"}, iplocationmatch.DefaultMaxCountries+1), false, codes.InvalidArgument},
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

// Every call is logged once with its result, gRPC code and, when resolved, the country.
func TestCheckIPLogging(t *testing.T) {
	tests := []struct {
		name        string
		ip          string
		countries   []string
		wantCode    string
		wantResult  string
		wantCountry string // "" means the field must be absent
	}{
		{"match", "81.2.69.142", []string{"GB"}, "OK", "match", "GB"},
		{"known, wrong country", "81.2.69.142", []string{"US"}, "OK", "no_match", "GB"},
		{"unknown ip", "8.8.8.8", []string{"US"}, "OK", "unknown_ip", ""},
		{"invalid ip", "nope", []string{"US"}, "InvalidArgument", "invalid", ""},
		{"no countries", "81.2.69.142", nil, "InvalidArgument", "invalid", ""},
		{"too many countries", "81.2.69.142", slices.Repeat([]string{"GB"}, iplocationmatch.DefaultMaxCountries+1), "InvalidArgument", "invalid", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, logs := newClient(t)
			_, _ = client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: tt.ip, AllowedCountries: tt.countries})

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want exactly 1: %s", len(lines), logs)
			}
			var l map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &l); err != nil {
				t.Fatalf("log line is not JSON: %v", err)
			}
			for k, want := range map[string]any{
				"msg": "request", "level": "INFO", "transport": "grpc", "code": tt.wantCode,
				"result": tt.wantResult, "ip": tt.ip, "db_version": "test-db",
			} {
				if l[k] != want {
					t.Errorf("%s = %v, want %v", k, l[k], want)
				}
			}
			if tt.wantCountry == "" {
				if _, ok := l["country"]; ok {
					t.Errorf("country must be absent, got %v", l["country"])
				}
			} else if l["country"] != tt.wantCountry {
				t.Errorf("country = %v, want %v", l["country"], tt.wantCountry)
			}
		})
	}
}

type failingLocator struct{}

func (failingLocator) Country(string) (string, error) {
	return "", errors.New("boom: secret internal detail")
}

func TestCheckIPInternalErrorIsOpaqueAndLogged(t *testing.T) {
	client, logs := serve(t, failingLocator{})
	_, err := client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: "1.1.1.1", AllowedCountries: []string{"US"}})

	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want opaque Internal", err)
	}
	var l map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &l); jerr != nil {
		t.Fatalf("log: %v: %s", jerr, logs)
	}
	if l["level"] != "ERROR" || l["code"] != "Internal" || l["result"] != "error" ||
		!strings.Contains(l["err"].(string), "secret internal detail") {
		t.Fatalf("unexpected log line: %v", l)
	}
}
