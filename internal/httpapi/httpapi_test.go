package httpapi_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/armenb/iplocationmatch/internal/httpapi"
	"github.com/armenb/iplocationmatch/internal/iplocationmatch"
	"github.com/armenb/iplocationmatch/internal/testdb"
)

func newHandler(t *testing.T, m httpapi.Matcher) http.Handler {
	t.Helper()
	if m == nil {
		c, err := iplocationmatch.Open(testdb.Write(t, testdb.Fixture))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		m = c
	}
	return httpapi.NewHandler(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		{"match", "POST", `{"ip":"81.2.69.142","countries":["GB","US"]}`, 200, `{"match":true}`},
		{"no match", "POST", `{"ip":"81.2.69.142","countries":["US"]}`, 200, `{"match":false}`},
		{"ip not in database", "POST", `{"ip":"8.8.8.8","countries":["US"]}`, 200, `{"match":false}`},
		{"ipv6", "POST", `{"ip":"2001:218::1","countries":["jp"]}`, 200, `{"match":true}`},
		{"invalid ip", "POST", `{"ip":"nope","countries":["US"]}`, 400, ""},
		{"missing ip", "POST", `{"countries":["US"]}`, 400, ""},
		{"invalid country", "POST", `{"ip":"81.2.69.142","countries":["USA"]}`, 400, ""},
		{"empty countries", "POST", `{"ip":"81.2.69.142","countries":[]}`, 400, ""},
		{"missing countries", "POST", `{"ip":"81.2.69.142"}`, 400, ""},
		{"malformed json", "POST", `{"ip":`, 400, ""},
		{"empty body", "POST", ``, 400, ""},
		{"unknown field", "POST", `{"ip":"81.2.69.142","countries":["GB"],"x":1}`, 400, ""},
		{"trailing data", "POST", `{"ip":"81.2.69.142","countries":["GB"]} {}`, 400, ""},
		{"wrong type", "POST", `{"ip":"81.2.69.142","countries":"GB"}`, 400, ""},
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

func TestCheckBodyTooLarge(t *testing.T) {
	h := newHandler(t, nil)
	body := `{"ip":"81.2.69.142","countries":["` + strings.Repeat("A", 2<<20) + `"]}`
	if rec := do(h, "POST", "/v1/check", body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

type failingMatcher struct{}

func (failingMatcher) InCountries(string, []string) (bool, error) {
	return false, errors.New("boom: secret internal detail")
}

func TestCheckInternalErrorIsOpaque(t *testing.T) {
	h := newHandler(t, failingMatcher{})
	rec := do(h, "POST", "/v1/check", `{"ip":"1.1.1.1","countries":["US"]}`)
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
