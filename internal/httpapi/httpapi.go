// Package httpapi exposes the country check as a JSON-over-HTTP endpoint.
// The contract is described in api/openapi.yaml.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"iplocationmatchservice/internal/iplocationmatch"
	"iplocationmatchservice/internal/telemetry"
)

// maxBodyBytes bounds request bodies; a realistic request is well under 1 KiB.
const maxBodyBytes = 1 << 20

// Locator resolves an IP address to a country; *iplocationmatch.Checker satisfies it.
type Locator interface {
	Country(ip string) (string, error)
}

// CheckRequest is the body of POST /v1/check.
type CheckRequest struct {
	IP               string   `json:"ip"`
	AllowedCountries []string `json:"allowed_countries"`
}

// CheckResponse is the success body of POST /v1/check.
type CheckResponse struct {
	Match bool `json:"match"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type handler struct {
	loc          Locator
	obs          *telemetry.Observer
	maxCountries int // longest allowed_countries list accepted
}

// NewHandler returns the service's HTTP routes. Requests whose allowed_countries
// has more than maxCountries entries are rejected. Every request except the
// health probe is logged and counted: each handler below starts an observation
// on entry and finishes it on every exit.
func NewHandler(loc Locator, obs *telemetry.Observer, maxCountries int) http.Handler {
	h := &handler{loc: loc, obs: obs, maxCountries: maxCountries}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/check", h.check)
	mux.HandleFunc("/v1/check", h.methodNotAllowed) // any other method
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/", h.notFound)
	return mux
}

// check answers POST /v1/check in explicit steps: decode, validate the
// allow-list, resolve the country, decide, respond.
func (h *handler) check(w http.ResponseWriter, r *http.Request) {
	obs := h.obs.Start("http")
	ctx := r.Context()
	req, bad := decodeRequest(w, r)

	if bad != nil {
		writeJSON(w, bad.status, errorResponse{bad.msg})
		obs.Finish(ctx, telemetry.Outcome{Code: strconv.Itoa(bad.status), Result: telemetry.ResultInvalid, IP: req.IP})
		return
	}
	if err := iplocationmatch.ValidateCountries(req.AllowedCountries, h.maxCountries); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{err.Error()})
		obs.Finish(ctx, telemetry.Outcome{Code: strconv.Itoa(http.StatusBadRequest), Result: telemetry.ResultInvalid, IP: req.IP})
		return
	}

	country, err := h.loc.Country(req.IP)
	switch {
	case iplocationmatch.IsInvalidInput(err):
		writeJSON(w, http.StatusBadRequest, errorResponse{err.Error()})
		obs.Finish(ctx, telemetry.Outcome{Code: strconv.Itoa(http.StatusBadRequest), Result: telemetry.ResultInvalid, IP: req.IP})
		return
	case err != nil:
		// The cause is logged, never returned to the caller.
		writeJSON(w, http.StatusInternalServerError, errorResponse{"internal error"})
		obs.Finish(ctx, telemetry.Outcome{Code: strconv.Itoa(http.StatusInternalServerError), Result: telemetry.ResultError, IP: req.IP, Err: err})
		return
	}

	match := iplocationmatch.InCountries(country, req.AllowedCountries) // unknown country: false
	writeJSON(w, http.StatusOK, CheckResponse{Match: match})
	obs.Finish(ctx, telemetry.Outcome{Code: strconv.Itoa(http.StatusOK), Result: telemetry.Classify(country, match), Country: country, IP: req.IP})
}

func (h *handler) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	obs := h.obs.Start("http")
	w.Header().Set("Allow", http.MethodPost)
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	obs.Finish(r.Context(), telemetry.Outcome{Code: strconv.Itoa(http.StatusMethodNotAllowed), Result: telemetry.ResultInvalid})
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	obs := h.obs.Start("http")
	http.NotFound(w, r)
	obs.Finish(r.Context(), telemetry.Outcome{Code: strconv.Itoa(http.StatusNotFound), Result: telemetry.ResultInvalid})
}

// badRequest is a request that cannot be processed, with the status to return.
type badRequest struct {
	status int
	msg    string
}

// decodeRequest strictly decodes the JSON body into a CheckRequest. The body must
// be one JSON object whose keys are exactly "ip" and "allowed_countries": names
// are case-sensitive and may not repeat, and unknown keys, trailing data and
// bodies over maxBodyBytes are rejected. (Go's encoding/json would match key
// names case-insensitively and let a repeated key silently win, so the keys are
// read token by token instead.) req is returned even on failure so the caller
// can still log whatever IP was read.
func decodeRequest(w http.ResponseWriter, r *http.Request) (req CheckRequest, bad *badRequest) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))

	malformed := func(msg string) (CheckRequest, *badRequest) {
		return req, &badRequest{http.StatusBadRequest, "malformed JSON body: " + msg}
	}
	fail := func(err error) (CheckRequest, *badRequest) {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return req, &badRequest{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return malformed(err.Error())
	}

	tok, err := dec.Token()
	if err != nil {
		return fail(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return malformed("expected a JSON object")
	}

	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fail(err)
		}
		key, ok := tok.(string)
		if !ok {
			return malformed("expected a field name")
		}
		if seen[key] {
			return malformed(fmt.Sprintf("duplicate field %q", key))
		}
		seen[key] = true

		switch key {
		case "ip":
			err = dec.Decode(&req.IP)
		case "allowed_countries":
			err = dec.Decode(&req.AllowedCountries)
		default:
			return malformed(fmt.Sprintf("unknown field %q", key))
		}
		if err != nil {
			return fail(err)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return fail(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, &badRequest{http.StatusBadRequest, "unexpected data after JSON body"}
	}
	return req, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // client disconnects are not actionable here
}
