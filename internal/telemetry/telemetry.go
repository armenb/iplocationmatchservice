// Package telemetry observes requests: one log line and a pair of metrics per
// request, for both transports, so the two can never drift apart.
//
// A handler starts an Observation when a request arrives and finishes it, with
// an explicit Outcome, on every exit:
//
//	obs := observer.Start("http")
//	...
//	obs.Finish(ctx, telemetry.Outcome{Code: "200", Result: telemetry.ResultMatch, Country: "GB", IP: ip})
//
// Cost: log and metric volume scale with request volume, which can be one call
// per HTTP resource a user loads, not one per login. Keep what is recorded
// compact: one short line per request, a handful of low-cardinality tags, and
// never the (potentially long) country list or the request body.
package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// Result classifies a request. Metric tags use these values, so the set stays small.
const (
	ResultMatch     = "match"      // known location, in an allowed country
	ResultNoMatch   = "no_match"   // known location, not in an allowed country
	ResultUnknownIP = "unknown_ip" // no country known for the IP; treated as not allowed
	ResultInvalid   = "invalid"    // client error (bad input)
	ResultError     = "error"      // server error
)

const (
	metricRequests = "iplocationmatch.requests"
	metricDuration = "iplocationmatch.request.duration"

	// maxIPLen caps how much of a caller-supplied IP string is logged. The
	// longest valid textual address (IPv6 with an IPv4 tail) is 45 bytes.
	maxIPLen = 64
)

// Sink receives metrics. *statsd.Client implements it; nil disables metrics.
type Sink interface {
	Incr(name string, tags ...string)
	Timing(name string, d time.Duration, tags ...string)
}

// Outcome describes how a request ended. Its transport and duration come from
// the Observation.
type Outcome struct {
	Code    string // HTTP status ("200") or gRPC code name ("OK")
	Result  string // one of the Result* constants
	Country string // resolved country (ISO alpha-2); "" if unknown or never looked up
	IP      string // as supplied by the caller; may be empty or invalid
	Err     error  // the cause, set only when Result is ResultError
}

// Observer logs and counts requests. It is safe for concurrent use.
type Observer struct {
	log       *slog.Logger
	sink      Sink
	dbVersion string
	now       func() time.Time // replaced in tests
}

// NewObserver returns an Observer. sink may be nil to disable metrics. dbVersion
// (see iplocationmatch.Checker.DBVersion) is stamped on every log line; the
// metrics client carries it as a constant tag.
func NewObserver(log *slog.Logger, sink Sink, dbVersion string) *Observer {
	return &Observer{log: log, sink: sink, dbVersion: dbVersion, now: time.Now}
}

// Observation is one request in flight. Finish it exactly once.
type Observation struct {
	o         *Observer
	transport string
	start     time.Time
}

// Start begins observing a request on transport ("http" or "grpc"), starting its clock.
func (o *Observer) Start(transport string) Observation {
	return Observation{o: o, transport: transport, start: o.now()}
}

// Finish logs the request and updates metrics.
func (ob Observation) Finish(ctx context.Context, out Outcome) {
	o := ob.o
	dur := o.now().Sub(ob.start)

	ip := out.IP
	if len(ip) > maxIPLen {
		ip = ip[:maxIPLen]
	}
	attrs := []slog.Attr{
		slog.String("transport", ob.transport),
		slog.String("code", out.Code),
		slog.String("result", out.Result),
	}
	if out.Country != "" { // two bytes; says why a user was denied. Never a metric tag: ~250 values.
		attrs = append(attrs, slog.String("country", out.Country))
	}
	attrs = append(attrs,
		slog.String("ip", ip),
		slog.String("db_version", o.dbVersion),
		slog.Int64("dur_us", dur.Microseconds()),
	)
	level := slog.LevelInfo
	if out.Err != nil {
		level = slog.LevelError
		attrs = append(attrs, slog.String("err", out.Err.Error()))
	}
	o.log.LogAttrs(ctx, level, "request", attrs...)

	if o.sink != nil {
		o.sink.Incr(metricRequests, "transport:"+ob.transport, "result:"+out.Result)
		o.sink.Timing(metricDuration, dur, "transport:"+ob.transport)
	}
}

// Classify names the result of a successful lookup: country is the resolved
// country ("" if unknown) and match whether it was allowed. Callers choose
// ResultInvalid and ResultError themselves, where they handle the error.
func Classify(country string, match bool) string {
	switch {
	case country == "":
		return ResultUnknownIP
	case match:
		return ResultMatch
	default:
		return ResultNoMatch
	}
}
