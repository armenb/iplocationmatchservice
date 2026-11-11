// Package iplocationmatch answers "is this IP address in one of these countries?" using
// a MaxMind GeoLite2-Country (.mmdb) database.
package iplocationmatch

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Errors returned for invalid caller input. Transports map these to
// 400 Bad Request / INVALID_ARGUMENT.
var (
	ErrInvalidIP      = errors.New("invalid IP address")
	ErrInvalidCountry = errors.New("invalid country code: want ISO 3166-1 alpha-2, e.g. \"US\"")
	ErrNoCountries    = errors.New("at least one country is required")
)

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

// InCountries reports whether ip is located in any of countries (ISO 3166-1
// alpha-2 codes, case-insensitive). An address absent from the database
// matches no country and yields (false, nil).
func (c *Checker) InCountries(ip string, countries []string) (bool, error) {
	addr, err := parseIP(ip)
	if err != nil {
		return false, err
	}
	want, err := countrySet(countries)
	if err != nil {
		return false, err
	}

	res := c.db.Lookup(addr)
	if err := res.Err(); err != nil {
		return false, fmt.Errorf("lookup %s: %w", addr, err)
	}
	if !res.Found() {
		return false, nil
	}
	var code string
	if err := res.DecodePath(&code, "country", "iso_code"); err != nil {
		return false, fmt.Errorf("decode country for %s: %w", addr, err)
	}
	_, ok := want[code]
	return ok, nil
}

func parseIP(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %q", ErrInvalidIP, s)
	}
	// Normalise IPv4-mapped IPv6 (::ffff:1.2.3.4) and drop any zone so the
	// address resolves in the IPv4 space of the database.
	return addr.Unmap().WithZone(""), nil
}

func countrySet(countries []string) (map[string]struct{}, error) {
	if len(countries) == 0 {
		return nil, ErrNoCountries
	}
	set := make(map[string]struct{}, len(countries))
	for _, c := range countries {
		if len(c) != 2 || !isASCIILetter(c[0]) || !isASCIILetter(c[1]) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidCountry, c)
		}
		set[strings.ToUpper(c)] = struct{}{}
	}
	return set, nil
}

func isASCIILetter(b byte) bool { return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' }
