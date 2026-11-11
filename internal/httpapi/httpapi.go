// Package httpapi exposes the country check as a JSON-over-HTTP endpoint.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/armenb/iplocationmatch/internal/iplocationmatch"
)

// maxBodyBytes bounds request bodies; a realistic request is well under 1 KiB.
const maxBodyBytes = 1 << 20

// Matcher is the lookup the handler depends on; *iplocationmatch.Checker satisfies it.
type Matcher interface {
	InCountries(ip string, countries []string) (bool, error)
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

// NewHandler returns the service's HTTP routes.
func NewHandler(m Matcher, log *slog.Logger) http.Handler {
	h := &handler{m: m, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/check", h.check)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

type handler struct {
	m   Matcher
	log *slog.Logger
}

func (h *handler) check(w http.ResponseWriter, r *http.Request) {
	var req CheckRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{"request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{"malformed JSON body: " + err.Error()})
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorResponse{"unexpected data after JSON body"})
		return
	}

	match, err := h.m.InCountries(req.IP, req.AllowedCountries)
	switch {
	case errors.Is(err, iplocationmatch.ErrInvalidIP),
		errors.Is(err, iplocationmatch.ErrInvalidCountry),
		errors.Is(err, iplocationmatch.ErrNoCountries):
		writeJSON(w, http.StatusBadRequest, errorResponse{err.Error()})
	case err != nil:
		h.log.ErrorContext(r.Context(), "lookup failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{"internal error"})
	default:
		writeJSON(w, http.StatusOK, CheckResponse{Match: match})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // client disconnects are not actionable here
}
