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
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	iplocationmatchv1 "iplocationmatchservice/gen/iplocationmatch/v1"
	"iplocationmatchservice/internal/testdb"
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
	if out, err := exec.Command("go", "build", "-o", binPath, "iplocationmatchservice/cmd/iplocationmatch").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		_ = os.RemoveAll(dir) // temp-dir cleanup; not actionable
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
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
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// server is a running iplocationmatch process.
type server struct {
	httpAddr, grpcAddr string
	stderr             *bytes.Buffer // complete only after Stop returns
	stop               func()
}

// Stop sends SIGTERM, waits for a clean exit, and fails the test otherwise.
// It is idempotent and also runs on cleanup.
func (s *server) Stop() { s.stop() }

// startServer launches iplocationmatch on dbPath with any extra flags, waits
// until it is healthy, and returns it. The process is stopped on cleanup.
func startServer(t *testing.T, dbPath string, extraArgs ...string) *server {
	t.Helper()

	srv := &server{httpAddr: freeAddr(t), grpcAddr: freeAddr(t), stderr: &bytes.Buffer{}}
	args := append([]string{"-db", dbPath, "-http-addr", srv.httpAddr, "-grpc-addr", srv.grpcAddr}, extraArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Stderr = srv.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start iplocationmatch: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var once sync.Once
	srv.stop = func() {
		once.Do(func() {
			// Under concurrent load the client can leave an idle connection that never sent
			// a request; http.Server.Shutdown waits up to 5s for such a connection, so close
			// ours first to keep the shutdown fast.
			http.DefaultClient.CloseIdleConnections()
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-exited:
				if err != nil {
					t.Errorf("iplocationmatch did not exit cleanly on SIGTERM: %v\n%s", err, srv.stderr.String())
				}
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				t.Errorf("iplocationmatch did not exit within 15s of SIGTERM\n%s", srv.stderr.String())
			}
		})
	}
	t.Cleanup(srv.stop)

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + srv.httpAddr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return srv
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("iplocationmatch exited during startup: %v\n%s", err, srv.stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("iplocationmatch not healthy within 10s\n%s", srv.stderr.String())
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
	srv := startServer(t, ds.dbPath)
	httpAddr, grpcAddr := srv.httpAddr, srv.grpcAddr

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
		defer func() { _ = resp.Body.Close() }()
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
							_ = resp.Body.Close()
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

// TestTelemetry drives requests over both transports and checks what the real
// binary reports: one request log line each (health probes excluded) and
// DogStatsD metrics tagged with the database version.
func TestTelemetry(t *testing.T) {
	agent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })

	srv := startServer(t, testdb.Write(t, testdb.Fixture), "-statsd-addr", agent.LocalAddr().String())

	conn, err := grpc.NewClient(srv.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := iplocationmatchv1.NewIPLocationMatchServiceClient(conn)

	post := func(body string) {
		t.Helper()
		resp, err := http.Post("http://"+srv.httpAddr+"/v1/check", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	// Sequential, so log order is deterministic.
	post(`{"ip":"81.2.69.142","allowed_countries":["GB"]}`) // http match
	if _, err := client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: "81.2.69.142", AllowedCountries: []string{"US"}}); err != nil {
		t.Fatal(err) // grpc no_match
	}
	post(`{"ip":"nope","allowed_countries":["US"]}`)    // http invalid
	post(`{"ip":"8.8.8.8","allowed_countries":["US"]}`) // http unknown_ip
	srv.Stop()                                          // flushes metrics, completes stderr

	// --- logs ---
	type req struct{ transport, code, result, country string }
	var got []req
	version := ""
	for line := range strings.SplitSeq(strings.TrimSpace(srv.stderr.String()), "\n") {
		var l map[string]any
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("stderr line is not JSON: %v: %s", err, line)
		}
		if l["msg"] != "request" {
			continue
		}
		country, _ := l["country"].(string) // absent when the location is unknown
		got = append(got, req{l["transport"].(string), l["code"].(string), l["result"].(string), country})
		v := l["db_version"].(string)
		if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(v) || (version != "" && v != version) {
			t.Fatalf("db_version = %q (previous %q)", v, version)
		}
		version = v
		if _, ok := l["dur_us"].(float64); !ok {
			t.Fatalf("dur_us missing: %s", line)
		}
	}
	want := []req{
		{"http", "200", "match", "GB"},
		{"grpc", "OK", "no_match", "GB"},
		{"http", "400", "invalid", ""},
		{"http", "200", "unknown_ip", ""},
	}
	if !slices.Equal(got, want) { // also proves the /healthz startup probes were not logged
		t.Fatalf("request log lines = %+v, want %+v", got, want)
	}

	// --- metrics ---
	var lines []string
	buf := make([]byte, 65535)
	for {
		_ = agent.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _, err := agent.ReadFromUDP(buf)
		if err != nil {
			break
		}
		lines = append(lines, strings.Split(string(buf[:n]), "\n")...)
	}
	tag := "|#db_version:" + version
	for _, w := range []string{
		"iplocationmatch.requests:1|c" + tag + ",transport:http,result:match",
		"iplocationmatch.requests:1|c" + tag + ",transport:grpc,result:no_match",
		"iplocationmatch.requests:1|c" + tag + ",transport:http,result:invalid",
		"iplocationmatch.requests:1|c" + tag + ",transport:http,result:unknown_ip",
	} {
		if !slices.Contains(lines, w) {
			t.Errorf("missing metric %q in %q", w, lines)
		}
	}
	timing := regexp.MustCompile(`^iplocationmatch\.request\.duration:\d+\.\d{3}\|ms` + regexp.QuoteMeta(tag) + `,transport:(http|grpc)$`)
	var nHTTP, nGRPC int
	for _, l := range lines {
		if m := timing.FindStringSubmatch(l); m != nil {
			if m[1] == "http" {
				nHTTP++
			} else {
				nGRPC++
			}
		}
	}
	if nHTTP != 3 || nGRPC != 1 {
		t.Errorf("timing metrics http=%d grpc=%d, want 3 and 1; lines: %q", nHTTP, nGRPC, lines)
	}
}

// TestAllowedCountriesLimit checks that -max-allowed-countries reaches both
// transports of the real binary.
func TestAllowedCountriesLimit(t *testing.T) {
	srv := startServer(t, testdb.Write(t, testdb.Fixture), "-max-allowed-countries", "2")

	post := func(countries string) (int, string) {
		t.Helper()
		resp, err := http.Post("http://"+srv.httpAddr+"/v1/check", "application/json",
			bytes.NewBufferString(`{"ip":"81.2.69.142","allowed_countries":[`+countries+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out bytes.Buffer
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}
	if code, body := post(`"GB","US"`); code != 200 {
		t.Fatalf("at the limit: %d %s", code, body)
	}
	if code, body := post(`"GB","US","SE"`); code != 400 || !strings.Contains(body, "got 3, at most 2 allowed") {
		t.Fatalf("over the limit: %d %s", code, body)
	}

	conn, err := grpc.NewClient(srv.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := iplocationmatchv1.NewIPLocationMatchServiceClient(conn)
	if _, err := client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: "81.2.69.142", AllowedCountries: []string{"GB", "US"}}); err != nil {
		t.Fatalf("grpc at the limit: %v", err)
	}
	_, err = client.CheckIP(t.Context(), &iplocationmatchv1.CheckIPRequest{Ip: "81.2.69.142", AllowedCountries: []string{"GB", "US", "SE"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("grpc over the limit: %v, want InvalidArgument", err)
	}
}

// A malformed or non-positive limit must stop startup, not fall back silently.
func TestInvalidLimitConfigFailsStartup(t *testing.T) {
	db := testdb.Write(t, testdb.Fixture)
	tests := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{"zero flag", []string{"-max-allowed-countries", "0"}, nil, "max-allowed-countries must be at least 1"},
		{"negative flag", []string{"-max-allowed-countries", "-3"}, nil, "max-allowed-countries must be at least 1"},
		{"non-numeric env", nil, []string{"IPLOCATIONMATCH_MAX_ALLOWED_COUNTRIES=lots"}, "IPLOCATIONMATCH_MAX_ALLOWED_COUNTRIES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-db", db, "-http-addr", "127.0.0.1:0", "-grpc-addr", "127.0.0.1:0"}, tt.args...)
			cmd := exec.Command(binPath, args...)
			cmd.Env = append(os.Environ(), tt.env...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected startup to fail; output: %s", out)
			}
			if !strings.Contains(string(out), tt.want) {
				t.Fatalf("output %q does not mention %q", out, tt.want)
			}
		})
	}
}
