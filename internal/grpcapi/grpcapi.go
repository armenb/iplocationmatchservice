// Package grpcapi exposes the country check as a gRPC service.
package grpcapi

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	iplocationmatchv1 "iplocationmatchservice/gen/iplocationmatch/v1"
	"iplocationmatchservice/internal/iplocationmatch"
	"iplocationmatchservice/internal/telemetry"
)

// Locator resolves an IP address to a country; *iplocationmatch.Checker satisfies it.
type Locator interface {
	Country(ip string) (string, error)
}

// Server implements iplocationmatchv1.IPLocationMatchServiceServer.
type Server struct {
	iplocationmatchv1.UnimplementedIPLocationMatchServiceServer
	loc          Locator
	obs          *telemetry.Observer
	maxCountries int // longest allowed_countries list accepted
}

// NewServer returns a IPLocationMatchService backed by loc. Requests whose
// allowed_countries has more than maxCountries entries are rejected. Every
// CheckIP call is logged and counted through obs.
func NewServer(loc Locator, obs *telemetry.Observer, maxCountries int) *Server {
	return &Server{loc: loc, obs: obs, maxCountries: maxCountries}
}

// CheckIP answers in explicit steps: validate the allow-list, resolve the
// country, decide, respond. It starts an observation on entry and finishes it
// on every exit.
func (s *Server) CheckIP(ctx context.Context, req *iplocationmatchv1.CheckIPRequest) (*iplocationmatchv1.CheckIPResponse, error) {
	obs := s.obs.Start("grpc")
	ip := req.GetIp()

	if err := iplocationmatch.ValidateCountries(req.GetAllowedCountries(), s.maxCountries); err != nil {
		obs.Finish(ctx, telemetry.Outcome{Code: codes.InvalidArgument.String(), Result: telemetry.ResultInvalid, IP: ip})
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	country, err := s.loc.Country(ip)
	switch {
	case iplocationmatch.IsInvalidInput(err):
		obs.Finish(ctx, telemetry.Outcome{Code: codes.InvalidArgument.String(), Result: telemetry.ResultInvalid, IP: ip})
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		// The cause is logged, never returned to the caller.
		obs.Finish(ctx, telemetry.Outcome{Code: codes.Internal.String(), Result: telemetry.ResultError, IP: ip, Err: err})
		return nil, status.Error(codes.Internal, "internal error")
	}

	match := iplocationmatch.InCountries(country, req.GetAllowedCountries()) // unknown country: false
	obs.Finish(ctx, telemetry.Outcome{Code: codes.OK.String(), Result: telemetry.Classify(country, match), Country: country, IP: ip})
	return &iplocationmatchv1.CheckIPResponse{Match: match}, nil
}
