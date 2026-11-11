package iplocationmatch_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"iplocationmatchservice/internal/iplocationmatch"
	"iplocationmatchservice/internal/testdb"
)

func newChecker(t *testing.T) *iplocationmatch.Checker {
	t.Helper()
	c, err := iplocationmatch.Open(testdb.Write(t, testdb.Fixture))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// countries returns n distinct valid two-letter codes (AA, AB, ...).
func countries(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%c%c", 'A'+byte(i/26%26), 'A'+byte(i%26))
	}
	return out
}

func TestCountry(t *testing.T) {
	c := newChecker(t)

	tests := []struct {
		name    string
		ip      string
		want    string
		wantErr error
	}{
		{"ipv4", "81.2.69.142", "GB", nil},
		{"another ipv4", "216.160.83.10", "US", nil},
		{"ipv6", "2001:218:1::1", "JP", nil},
		{"ipv4-mapped ipv6 resolves as ipv4", "::ffff:81.2.69.142", "GB", nil},
		{"surrounding whitespace rejected", " 81.2.69.142 ", "", iplocationmatch.ErrInvalidIP},
		{"ip absent from database", "8.8.8.8", "", nil},
		// This record's registered_country is US; it must not be used as the location.
		{"record without a country", "203.0.113.5", "", nil},
		{"invalid ip", "not-an-ip", "", iplocationmatch.ErrInvalidIP},
		{"empty ip", "", "", iplocationmatch.ErrInvalidIP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.Country(tt.ip)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Country = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateCountries(t *testing.T) {
	tests := []struct {
		name      string
		countries []string
		wantErr   error
	}{
		{"one code", []string{"GB"}, nil},
		{"several, mixed case", []string{"gb", "US", "Se"}, nil},
		{"nil", nil, iplocationmatch.ErrNoCountries},
		{"empty", []string{}, iplocationmatch.ErrNoCountries},
		{"too long", []string{"USA"}, iplocationmatch.ErrInvalidCountry},
		{"not letters", []string{"U1"}, iplocationmatch.ErrInvalidCountry},
		{"one bad entry rejects the list", []string{"GB", ""}, iplocationmatch.ErrInvalidCountry},
		{"exactly at the limit", countries(iplocationmatch.DefaultMaxCountries), nil},
		{"one over the limit", countries(iplocationmatch.DefaultMaxCountries + 1), iplocationmatch.ErrTooManyCountries},
		// The length is checked before the entries, so a long list of junk is rejected cheaply.
		{"over the limit, entries not examined", slices.Repeat([]string{"?"}, iplocationmatch.DefaultMaxCountries+1), iplocationmatch.ErrTooManyCountries},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := iplocationmatch.ValidateCountries(tt.countries, iplocationmatch.DefaultMaxCountries); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateCountriesLimitIsConfigurable(t *testing.T) {
	err := iplocationmatch.ValidateCountries([]string{"GB", "US", "SE"}, 2)
	if !errors.Is(err, iplocationmatch.ErrTooManyCountries) {
		t.Fatalf("err = %v, want ErrTooManyCountries", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "got 3") || !strings.Contains(msg, "at most 2") {
		t.Fatalf("message %q should say how many were sent and the limit", msg)
	}
	if err := iplocationmatch.ValidateCountries([]string{"GB", "US"}, 2); err != nil {
		t.Fatalf("a list exactly at the limit must be accepted: %v", err)
	}
}

func TestInCountries(t *testing.T) {
	tests := []struct {
		name    string
		country string
		allowed []string
		want    bool
	}{
		{"listed", "GB", []string{"GB"}, true},
		{"among several", "US", []string{"CA", "US", "MX"}, true},
		{"case-insensitive", "SE", []string{"se"}, true},
		{"not listed", "GB", []string{"US", "SE"}, false},
		// Unknown locations fail closed: never allowed, whatever the list says.
		{"unknown country", "", []string{"US", "GB"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := iplocationmatch.InCountries(tt.country, tt.allowed); got != tt.want {
				t.Fatalf("InCountries(%q, %v) = %v, want %v", tt.country, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestIsInvalidInput(t *testing.T) {
	c := newChecker(t)

	_, ipErr := c.Country("nope")
	countriesErr := iplocationmatch.ValidateCountries([]string{"USA"}, iplocationmatch.DefaultMaxCountries)
	noCountriesErr := iplocationmatch.ValidateCountries(nil, iplocationmatch.DefaultMaxCountries)
	tooManyErr := iplocationmatch.ValidateCountries(countries(iplocationmatch.DefaultMaxCountries+1), iplocationmatch.DefaultMaxCountries)
	for name, err := range map[string]error{"bad ip": ipErr, "bad country": countriesErr, "no countries": noCountriesErr, "too many": tooManyErr} {
		if !iplocationmatch.IsInvalidInput(err) {
			t.Errorf("%s: IsInvalidInput(%v) = false, want true", name, err)
		}
	}
	if iplocationmatch.IsInvalidInput(nil) || iplocationmatch.IsInvalidInput(errors.New("disk on fire")) {
		t.Fatal("nil and server-side errors are not invalid input")
	}
}

func TestDBVersion(t *testing.T) {
	v := newChecker(t).DBVersion()
	if _, err := time.Parse("2006-01-02", v); err != nil {
		t.Fatalf("DBVersion %q is not a YYYY-MM-DD date: %v", v, err)
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := iplocationmatch.Open("/nonexistent/GeoLite2-Country.mmdb"); err == nil {
		t.Fatal("expected error opening missing database")
	}
}

// Run with -race: many goroutines share one Checker.
func TestCountryConcurrent(t *testing.T) {
	c := newChecker(t)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			for range 200 {
				ip, want := "81.2.69.142", "GB"
				if i%2 == 1 {
					ip, want = "216.160.83.1", "US"
				}
				got, err := c.Country(ip)
				if err != nil || got != want {
					t.Errorf("Country(%s) = %q, %v; want %q", ip, got, err, want)
					return
				}
			}
		})
	}
	wg.Wait()
}
