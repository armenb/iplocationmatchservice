package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"iplocationmatchservice/internal/telemetry"
)

type fakeSink struct{ calls []string }

func (f *fakeSink) Incr(name string, tags ...string) {
	f.calls = append(f.calls, fmt.Sprintf("incr %s %v", name, tags))
}

func (f *fakeSink) Timing(name string, d time.Duration, tags ...string) {
	f.calls = append(f.calls, fmt.Sprintf("timing %s %v %v", name, d, tags))
}

// finish starts an observation on transport, lets dur pass on a fake clock,
// finishes it with out, and returns the parsed log line.
func finish(t *testing.T, sink telemetry.Sink, transport string, dur time.Duration, out telemetry.Outcome) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	obs := telemetry.NewObserver(slog.New(slog.NewJSONHandler(&buf, nil)), sink, "2026-11-10")

	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ticks := []time.Time{clock, clock.Add(dur)} // Start, then Finish
	obs.SetClock(func() time.Time { n := ticks[0]; ticks = ticks[1:]; return n })

	obs.Start(transport).Finish(context.Background(), out)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	return line
}

func TestFinishLogLine(t *testing.T) {
	line := finish(t, nil, "http", 1234*time.Microsecond, telemetry.Outcome{
		Code: "200", Result: telemetry.ResultMatch, Country: "GB", IP: "81.2.69.142",
	})

	want := map[string]any{
		"msg": "request", "level": "INFO",
		"transport": "http", "code": "200", "result": "match", "country": "GB",
		"ip": "81.2.69.142", "db_version": "2026-11-10", "dur_us": float64(1234),
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if _, ok := line["err"]; ok {
		t.Error("err must be absent when there is no error")
	}
}

// An unknown or unresolved country adds nothing to the line (compactness).
func TestFinishOmitsEmptyCountry(t *testing.T) {
	line := finish(t, nil, "http", 0, telemetry.Outcome{Code: "200", Result: telemetry.ResultUnknownIP, IP: "8.8.8.8"})
	if _, ok := line["country"]; ok {
		t.Fatalf("country must be absent when empty: %v", line)
	}
}

func TestFinishErrorIsLoggedAtErrorLevel(t *testing.T) {
	line := finish(t, nil, "grpc", 0, telemetry.Outcome{
		Code: "Internal", Result: telemetry.ResultError, IP: "1.1.1.1", Err: errors.New("lookup failed"),
	})
	if line["level"] != "ERROR" || line["err"] != "lookup failed" {
		t.Fatalf("level/err = %v/%v", line["level"], line["err"])
	}
}

// The IP is caller-supplied text; an oversized value must not bloat the log.
func TestFinishTruncatesIP(t *testing.T) {
	line := finish(t, nil, "http", 0, telemetry.Outcome{Code: "400", Result: telemetry.ResultInvalid, IP: strings.Repeat("9", 10_000)})
	if got := line["ip"].(string); len(got) != 64 {
		t.Fatalf("logged ip length = %d, want 64", len(got))
	}
}

func TestFinishMetrics(t *testing.T) {
	sink := &fakeSink{}
	finish(t, sink, "grpc", 2*time.Millisecond, telemetry.Outcome{Code: "OK", Result: telemetry.ResultNoMatch})

	want := []string{
		"incr iplocationmatch.requests [transport:grpc result:no_match]",
		"timing iplocationmatch.request.duration 2ms [transport:grpc]",
	}
	if !slices.Equal(sink.calls, want) {
		t.Fatalf("calls = %q, want %q", sink.calls, want)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		country string
		match   bool
		want    string
	}{
		{"allowed", "GB", true, telemetry.ResultMatch},
		{"known, not allowed", "GB", false, telemetry.ResultNoMatch},
		{"unknown location", "", false, telemetry.ResultUnknownIP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := telemetry.Classify(tt.country, tt.match); got != tt.want {
				t.Fatalf("Classify(%q, %v) = %q, want %q", tt.country, tt.match, got, tt.want)
			}
		})
	}
}
