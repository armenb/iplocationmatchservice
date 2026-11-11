// Package grpcapi exposes the country check as a gRPC service.
package grpcapi

import (
	"context"
	"errors"
	"log/slog"

	iplocationmatchv1 "github.com/armenb/iplocationmatch/gen/iplocationmatch/v1"
	"github.com/armenb/iplocationmatch/internal/iplocationmatch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Matcher is the lookup the server depends on; *iplocationmatch.Checker satisfies it.
type Matcher interface {
	InCountries(ip string, countries []string) (bool, error)
}

// Server implements iplocationmatchv1.IPLocationMatchServiceServer.
type Server struct {
	iplocationmatchv1.UnimplementedIPLocationMatchServiceServer
	m   Matcher
	log *slog.Logger
}

// NewServer returns a IPLocationMatchService backed by m.
func NewServer(m Matcher, log *slog.Logger) *Server {
	return &Server{m: m, log: log}
}

// CheckIP implements iplocationmatchv1.IPLocationMatchServiceServer.
func (s *Server) CheckIP(ctx context.Context, req *iplocationmatchv1.CheckIPRequest) (*iplocationmatchv1.CheckIPResponse, error) {
	match, err := s.m.InCountries(req.GetIp(), req.GetAllowedCountries())
	switch {
	case errors.Is(err, iplocationmatch.ErrInvalidIP),
		errors.Is(err, iplocationmatch.ErrInvalidCountry),
		errors.Is(err, iplocationmatch.ErrNoCountries):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		s.log.ErrorContext(ctx, "lookup failed", "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &iplocationmatchv1.CheckIPResponse{Match: match}, nil
}
