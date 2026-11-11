// Package iplocationmatch answers "is this IP address in one of these countries?" using
// a MaxMind GeoLite2-Country (.mmdb) database.
//
// The question is answered in three explicit steps, which callers run in order:
// ValidateCountries (the allow-list is well formed), Checker.Country (where is
// the IP?) and InCountries (is that country allowed?).
package iplocationmatch

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Errors returned for invalid caller input. Transports map these to
// 400 Bad Request / INVALID_ARGUMENT.
var (
	ErrInvalidIP      = errors.New("invalid IP address")
	ErrInvalidCountry = errors.New("invalid country code: want ISO 3166-1 alpha-2, e.g. \"US\"")
	ErrNoCountries    = errors.New("at least one country is required")
	// ErrTooManyCountries is returned for an allow-list longer than the limit. It is
	// wrapped with the number received and the limit, so a caller sees both.
	ErrTooManyCountries = errors.New("too many countries")
)

// DefaultMaxCountries is the default limit on the length of an allow-list: 50,
// which covers every country in Europe plus a handful of others. A caller who
// needs more is better served by working out whether they really want a higher
// limit, or an "all except these" variant, than by silently accepting long lists.
const DefaultMaxCountries = 50

// IsInvalidInput reports whether err was caused by the caller's input, as
// opposed to a server fault.
func IsInvalidInput(err error) bool {
	return errors.Is(err, ErrInvalidIP) ||
		errors.Is(err, ErrInvalidCountry) ||
		errors.Is(err, ErrNoCountries) ||
		errors.Is(err, ErrTooManyCountries)
}

// Checker looks up IP addresses in a read-only, memory-mapped MaxMind
// database. It is safe for concurrent use.
type Checker struct {
	db *maxminddb.Reader
}

// Open opens the .mmdb database at path. The file is memory-mapped read-only.
func Open(path string) (*Checker, error) {
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mmdb %q: %w", path, err)
	}
	return &Checker{db: db}, nil
}

// Close releases the database. Lookups must not be in flight or follow.
func (c *Checker) Close() error { return c.db.Close() }

// DBVersion identifies the loaded database by its build date (UTC,
// "2006-01-02"), for logs and metric tags.
func (c *Checker) DBVersion() string {
	return c.db.Metadata.BuildTime().UTC().Format("2006-01-02")
}

// Country returns the ISO 3166-1 alpha-2 code of the country ip is located in.
//
// It returns "" and a nil error when the database has no country for the
// address: it is absent (private and reserved ranges, unmapped networks), or its
// record has no country. Such an unknown location is never allowed (fail
// closed); InCountries enforces that.
func (c *Checker) Country(ip string) (string, error) {
	addr, err := parseIP(ip)
	if err != nil {
		return "", err
	}
	res := c.db.Lookup(addr)
	if err := res.Err(); err != nil {
		return "", fmt.Errorf("lookup %s: %w", addr, err)
	}
	if !res.Found() {
		return "", nil
	}
	var code string
	if err := res.DecodePath(&code, "country", "iso_code"); err != nil {
		return "", fmt.Errorf("decode country for %s: %w", addr, err)
	}
	return code, nil
}

// ValidateCountries checks an allow-list: it must be non-empty, have at most
// limit entries, and every entry must be a two-letter code. An empty list is
// rejected rather than interpreted: a caller with no restriction can see that
// and skip the call. The length is checked first, so an oversized list is
// rejected without examining its entries.
func ValidateCountries(countries []string, limit int) error {
	if len(countries) == 0 {
		return ErrNoCountries
	}
	if len(countries) > limit {
		return fmt.Errorf("%w: got %d, at most %d allowed", ErrTooManyCountries, len(countries), limit)
	}
	for _, c := range countries {
		if len(c) != 2 || !isASCIILetter(c[0]) || !isASCIILetter(c[1]) {
			return fmt.Errorf("%w: %q", ErrInvalidCountry, c)
		}
	}
	return nil
}

// InCountries reports whether country (as returned by Country) is one of
// allowed, compared case-insensitively. allowed must have passed
// ValidateCountries. An unknown country ("") is never allowed.
func InCountries(country string, allowed []string) bool {
	if country == "" {
		return false
	}
	return slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, country) })
}

func parseIP(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(s) // no TrimSpace: a padded IP is a malformed request, not something to guess at
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %q", ErrInvalidIP, s)
	}
	// Normalise IPv4-mapped IPv6 (::ffff:1.2.3.4) and drop any zone so the
	// address resolves in the IPv4 space of the database.
	return addr.Unmap().WithZone(""), nil
}

func isASCIILetter(b byte) bool { return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' }
