package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"iplocationmatchservice/internal/httpapi"
	"iplocationmatchservice/internal/iplocationmatch"
	"iplocationmatchservice/internal/telemetry"
	"iplocationmatchservice/internal/testdb"
)

// newObservedHandler returns the handler and a buffer receiving its JSON request log.
func newObservedHandler(t *testing.T, m httpapi.Locator) (http.Handler, *bytes.Buffer) {
	t.Helper()
	if m == nil {
		c, err := iplocationmatch.Open(testdb.Write(t, testdb.Fixture))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		m = c
	}
	var logs bytes.Buffer
	obs := telemetry.NewObserver(slog.New(slog.NewJSONHandler(&logs, nil)), nil, "test-db")
	return httpapi.NewHandler(m, obs, iplocationmatch.DefaultMaxCountries), &logs
}

func newHandler(t *testing.T, m httpapi.Locator) http.Handler {
	t.Helper()
	h, _ := newObservedHandler(t, m)
	return h
}

// bodyWithCountries is a request for 81.2.69.142 (GB) with n allowed_countries, all "GB".
func bodyWithCountries(n int) string {
	return `{"ip":"81.2.69.142","allowed_countries":[` + strings.TrimSuffix(strings.Repeat(`"GB",`, n), ",") + `]}`
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCheck(t *testing.T) {
	h := newHandler(t, nil)

	tests := []struct {
		name       string
		method     string
		body       string
		wantStatus int
		wantBody   string // exact JSON body, or "" to skip
	}{
		{"match", "POST", `{"ip":"81.2.69.142","allowed_countries":["GB","US"]}`, 200, `{"match":true}`},
		{"no match", "POST", `{"ip":"81.2.69.142","allowed_countries":["US"]}`, 200, `{"match":false}`},
		{"ip not in database", "POST", `{"ip":"8.8.8.8","allowed_countries":["US"]}`, 200, `{"match":false}`},
		{"ipv6", "POST", `{"ip":"2001:218::1","allowed_countries":["jp"]}`, 200, `{"match":true}`},
		{"invalid ip", "POST", `{"ip":"nope","allowed_countries":["US"]}`, 400, ""},
		{"missing ip", "POST", `{"allowed_countries":["US"]}`, 400, ""},
		{"invalid country", "POST", `{"ip":"81.2.69.142","allowed_countries":["USA"]}`, 400, ""},
		{"empty allowed_countries", "POST", `{"ip":"81.2.69.142","allowed_countries":[]}`, 400, ""},
		{"exactly at the limit", "POST", bodyWithCountries(iplocationmatch.DefaultMaxCountries), 200, `{"match":true}`},
		{"over the limit", "POST", bodyWithCountries(iplocationmatch.DefaultMaxCountries + 1), 400, ""},
		{"missing allowed_countries", "POST", `{"ip":"81.2.69.142"}`, 400, ""},
		{"malformed json", "POST", `{"ip":`, 400, ""},
		{"empty body", "POST", ``, 400, ""},
		{"unknown field", "POST", `{"ip":"81.2.69.142","allowed_countries":["GB"],"x":1}`, 400, ""},
		{"old field name rejected", "POST", `{"ip":"81.2.69.142","countries":["GB"]}`, 400, ""},
		{"keys in any order", "POST", `{"allowed_countries":["GB"],"ip":"81.2.69.142"}`, 200, `{"match":true}`},
		{"wrong-case keys", "POST", `{"IP":"81.2.69.142","ALLOWED_COUNTRIES":["GB"]}`, 400, ""},
		{"wrong-case extra key", "POST", `{"ip":"81.2.69.142","allowed_countries":["GB"],"Allowed_Countries":["US"]}`, 400, ""},
		{"duplicate ip", "POST", `{"ip":"1.1.1.1","ip":"81.2.69.142","allowed_countries":["GB"]}`, 400, ""},
		{"duplicate allowed_countries", "POST", `{"ip":"81.2.69.142","allowed_countries":["US"],"allowed_countries":["GB"]}`, 400, ""},
		{"padded ip", "POST", `{"ip":" 81.2.69.142 ","allowed_countries":["GB"]}`, 400, ""},
		{"null ip", "POST", `{"ip":null,"allowed_countries":["GB"]}`, 400, ""},
		{"body is an array", "POST", `[]`, 400, ""},
		{"body is a string", "POST", `"x"`, 400, ""},
		{"non-string country", "POST", `{"ip":"81.2.69.142","allowed_countries":["GB",1]}`, 400, ""},
		{"trailing data", "POST", `{"ip":"81.2.69.142","allowed_countries":["GB"]} {}`, 400, ""},
		{"wrong type", "POST", `{"ip":"81.2.69.142","allowed_countries":"GB"}`, 400, ""},
		{"GET not allowed", "GET", ``, 405, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(h, tt.method, "/v1/check", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantBody != "" {
				if got := strings.TrimSpace(rec.Body.String()); got != tt.wantBody {
					t.Fatalf("body = %q, want %q", got, tt.wantBody)
				}
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Fatalf("Content-Type = %q", ct)
				}
			}
		})
	}
}

func TestCheckTooManyCountriesMessage(t *testing.T) {
	rec := do(newHandler(t, nil), "POST", "/v1/check", bodyWithCountries(iplocationmatch.DefaultMaxCountries+1))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "got 51, at most 50 allowed") {
		t.Fatalf("status %d, body %q; want 400 saying how many were sent and the limit", rec.Code, rec.Body)
	}
}

// The message says what was wrong, so callers can fix the request.
func TestCheckRejectionMessages(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"duplicate key", `{"ip":"1.1.1.1","ip":"81.2.69.142","allowed_countries":["GB"]}`, `duplicate field \"ip\"`},
		{"wrong-case key", `{"IP":"81.2.69.142","allowed_countries":["GB"]}`, `unknown field \"IP\"`},
		{"not an object", `[]`, "expected a JSON object"},
		{"padded ip", `{"ip":" 81.2.69.142 ","allowed_countries":["GB"]}`, "invalid IP address"},
	}
	h := newHandler(t, nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(h, "POST", "/v1/check", tt.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("status %d, body %q; want 400 containing %q", rec.Code, rec.Body, tt.want)
			}
		})
	}
}

func TestCheckBodyTooLarge(t *testing.T) {
	h := newHandler(t, nil)
	body := `{"ip":"81.2.69.142","allowed_countries":["` + strings.Repeat("A", 2<<20) + `"]}`
	if rec := do(h, "POST", "/v1/check", body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

type failingLocator struct{}

func (failingLocator) Country(string) (string, error) {
	return "", errors.New("boom: secret internal detail")
}

func TestCheckInternalErrorIsOpaque(t *testing.T) {
	h := newHandler(t, failingLocator{})
	rec := do(h, "POST", "/v1/check", `{"ip":"1.1.1.1","allowed_countries":["US"]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("internal error detail leaked: %q", rec.Body)
	}
}

func TestHealthz(t *testing.T) {
	if rec := do(newHandler(t, nil), "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// logLines parses the JSON request log, one object per line.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, l)
		}
		out = append(out, m)
	}
	return out
}

// Every request is logged once with its result, status and, when resolved, the
// country, including requests the mux rejects before they reach check.
func TestRequestLogging(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		wantCode    string
		wantResult  string
		wantCountry string // "" means the field must be absent
		wantIP      string
	}{
		{"match", "POST", "/v1/check", `{"ip":"81.2.69.142","allowed_countries":["GB"]}`, "200", "match", "GB", "81.2.69.142"},
		{"known, wrong country", "POST", "/v1/check", `{"ip":"81.2.69.142","allowed_countries":["US"]}`, "200", "no_match", "GB", "81.2.69.142"},
		{"unknown ip", "POST", "/v1/check", `{"ip":"8.8.8.8","allowed_countries":["US"]}`, "200", "unknown_ip", "", "8.8.8.8"},
		{"invalid ip", "POST", "/v1/check", `{"ip":"nope","allowed_countries":["US"]}`, "400", "invalid", "", "nope"},
		{"empty allowed_countries", "POST", "/v1/check", `{"ip":"81.2.69.142","allowed_countries":[]}`, "400", "invalid", "", "81.2.69.142"},
		{"too many allowed_countries", "POST", "/v1/check", bodyWithCountries(iplocationmatch.DefaultMaxCountries + 1), "400", "invalid", "", "81.2.69.142"},
		{"malformed json", "POST", "/v1/check", `{"ip":`, "400", "invalid", "", ""},
		{"wrong method", "GET", "/v1/check", ``, "405", "invalid", "", ""},
		{"unknown path", "POST", "/v1/nope", `{}`, "404", "invalid", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, logs := newObservedHandler(t, nil)
			do(h, tt.method, tt.path, tt.body)

			lines := logLines(t, logs)
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want exactly 1: %s", len(lines), logs)
			}
			l := lines[0]
			for k, want := range map[string]any{
				"msg": "request", "level": "INFO", "transport": "http", "code": tt.wantCode,
				"result": tt.wantResult, "ip": tt.wantIP, "db_version": "test-db",
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
			if _, ok := l["dur_us"].(float64); !ok {
				t.Errorf("dur_us missing or not a number: %v", l["dur_us"])
			}
		})
	}
}

func TestHealthzIsNotLogged(t *testing.T) {
	h, logs := newObservedHandler(t, nil)
	do(h, "GET", "/healthz", "")
	if logs.Len() != 0 {
		t.Fatalf("health probe was logged: %s", logs)
	}
}

func TestInternalErrorLogsCause(t *testing.T) {
	h, logs := newObservedHandler(t, failingLocator{})
	do(h, "POST", "/v1/check", `{"ip":"1.1.1.1","allowed_countries":["US"]}`)

	lines := logLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %s", len(lines), logs)
	}
	if l := lines[0]; l["level"] != "ERROR" || l["code"] != "500" || l["result"] != "error" ||
		!strings.Contains(l["err"].(string), "secret internal detail") {
		t.Fatalf("unexpected log line: %v", l)
	}
}
