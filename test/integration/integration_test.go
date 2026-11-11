//go:build integration

// Package integration runs the real iplocationmatch binary and exercises both the HTTP
// and gRPC endpoints over TCP. Run with: go test -tags integration ./test/integration
//
// The suite always runs against a generated GeoLite2-style fixture. If
// IPLOCATIONMATCH_TEST_DB points at a real GeoLite2-Country.mmdb (see `make fetch-db`),
// it additionally runs against that file; see `make test-integration-real`.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	iplocationmatchv1 "iplocationmatch/gen/iplocationmatch/v1"
	"iplocationmatch/internal/testdb"
)

// neverACountry is a well-formed code that is not assigned to any country, so
// asking whether an IP is in it is a deterministic "no" for any database.
const neverACountry = "ZZ"

var binPath string

// TestMain builds iplocationmatch once for all tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iplocationmatch-it-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "iplocationmatch")
	if out, err := exec.Command("go", "build", "-o", binPath, "iplocationmatch/cmd/iplocationmatch").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type probe struct{ ip, country string }

// dataset is a database plus addresses whose country in that database is known.
type dataset struct {
	name   string
	dbPath string
	v4, v6 probe
}

func datasets(t *testing.T) []dataset {
	t.Helper()
	ds := []dataset{{
		name:   "fixture",
		dbPath: testdb.Write(t, testdb.Fixture),
		v4:     probe{"81.2.69.142", "GB"},
		v6:     probe{"2001:218::1", "JP"},
	}}
	if p := os.Getenv("IPLOCATIONMATCH_TEST_DB"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("IPLOCATIONMATCH_TEST_DB is set but unreadable: %v", err)
		}
		// Expected values are well-known public addresses; they depend on the
		// database release, so a mismatch after a MaxMind update is worth a look
		// rather than proof of a service bug.
		ds = append(ds, dataset{
			name:   "real",
			dbPath: p,
			v4:     probe{"8.8.8.8", "US"},
			v6:     probe{"2001:4860:4860::8888", "US"},
		})
	}
	return ds
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startServer launches iplocationmatch on dbPath, waits until it is healthy, and
// returns its HTTP and gRPC addresses. The process is stopped on cleanup.
func startServer(t *testing.T, dbPath string) (httpAddr, grpcAddr string) {
	t.Helper()

	httpAddr, grpcAddr = freeAddr(t), freeAddr(t)
	var stderr bytes.Buffer
	cmd := exec.Command(binPath, "-db", dbPath, "-http-addr", httpAddr, "-grpc-addr", grpcAddr)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start iplocationmatch: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("iplocationmatch did not exit cleanly on SIGTERM: %v\n%s", err, stderr.String())
			}
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Errorf("iplocationmatch did not exit within 15s of SIGTERM\n%s", stderr.String())
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + httpAddr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return httpAddr, grpcAddr
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("iplocationmatch exited during startup: %v\n%s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("iplocationmatch not healthy within 10s\n%s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestService(t *testing.T) {
	for _, ds := range datasets(t) {
		t.Run(ds.name, func(t *testing.T) { runSuite(t, ds) })
	}
}

func runSuite(t *testing.T, ds dataset) {
	httpAddr, grpcAddr := startServer(t, ds.dbPath)

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := iplocationmatchv1.NewIPLocationMatchServiceClient(conn)

	postCheck := func(t *testing.T, body string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post("http://"+httpAddr+"/v1/check", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp.StatusCode, out
	}

	t.Run("http", func(t *testing.T) {
		body := func(ip string, countries ...string) string {
			b, _ := json.Marshal(map[string]any{"ip": ip, "allowed_countries": countries})
			return string(b)
		}
		if code, out := postCheck(t, body(ds.v4.ip, neverACountry, ds.v4.country)); code != 200 || out["match"] != true {
			t.Fatalf("ipv4 match: %d %v", code, out)
		}
		if code, out := postCheck(t, body(ds.v6.ip, ds.v6.country)); code != 200 || out["match"] != true {
			t.Fatalf("ipv6 match: %d %v", code, out)
		}
		if code, out := postCheck(t, body(ds.v4.ip, neverACountry)); code != 200 || out["match"] != false {
			t.Fatalf("no match: %d %v", code, out)
		}
		if code, _ := postCheck(t, body("bogus", ds.v4.country)); code != 400 {
			t.Fatalf("invalid ip: status %d, want 400", code)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		resp, err := client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: ds.v4.ip, AllowedCountries: []string{neverACountry, ds.v4.country}})
		if err != nil || !resp.GetMatch() {
			t.Fatalf("ipv4 match: %v, %v", resp, err)
		}
		resp, err = client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: ds.v6.ip, AllowedCountries: []string{ds.v6.country}})
		if err != nil || !resp.GetMatch() {
			t.Fatalf("ipv6 match: %v, %v", resp, err)
		}
		resp, err = client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: ds.v4.ip, AllowedCountries: []string{neverACountry}})
		if err != nil || resp.GetMatch() {
			t.Fatalf("no match: %v, %v", resp, err)
		}
		_, err = client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: "bogus", AllowedCountries: []string{ds.v4.country}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid ip: %v, want InvalidArgument", err)
		}
	})

	t.Run("concurrent mixed load", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		cases := []struct {
			ip, country string
			want        bool
		}{
			{ds.v4.ip, ds.v4.country, true},
			{ds.v4.ip, neverACountry, false},
			{ds.v6.ip, ds.v6.country, true},
		}

		var wg sync.WaitGroup
		for i := range 32 {
			wg.Go(func() {
				for range 50 {
					// Even workers use HTTP, odd workers use gRPC.
					for _, tc := range cases {
						var got bool
						if i%2 == 0 {
							body := fmt.Sprintf(`{"ip":%q,"allowed_countries":[%q]}`, tc.ip, tc.country)
							req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+httpAddr+"/v1/check", bytes.NewBufferString(body))
							resp, err := http.DefaultClient.Do(req)
							if err != nil {
								t.Errorf("http: %v", err)
								return
							}
							var out struct{ Match bool }
							err = json.NewDecoder(resp.Body).Decode(&out)
							resp.Body.Close()
							if err != nil {
								t.Errorf("decode: %v", err)
								return
							}
							got = out.Match
						} else {
							r, err := client.CheckIP(ctx, &iplocationmatchv1.CheckIPRequest{Ip: tc.ip, AllowedCountries: []string{tc.country}})
							if err != nil {
								t.Errorf("grpc: %v", err)
								return
							}
							got = r.GetMatch()
						}
						if got != tc.want {
							t.Errorf("worker %d: %s in %s = %v, want %v", i, tc.ip, tc.country, got, tc.want)
							return
						}
					}
				}
			})
		}
		wg.Wait()
	})
}
