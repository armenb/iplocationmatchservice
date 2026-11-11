package iplocationmatch_test

import (
	"errors"
	"sync"
	"testing"

	"iplocationmatch/internal/iplocationmatch"
	"iplocationmatch/internal/testdb"
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

func TestInCountries(t *testing.T) {
	c := newChecker(t)

	tests := []struct {
		name      string
		ip        string
		countries []string
		want      bool
		wantErr   error
	}{
		{"ipv4 in listed country", "81.2.69.142", []string{"GB"}, true, nil},
		{"ipv4 among several countries", "216.160.83.10", []string{"CA", "US", "MX"}, true, nil},
		{"ipv4 in other country", "81.2.69.142", []string{"US", "SE"}, false, nil},
		{"lowercase country codes", "89.160.20.1", []string{"se"}, true, nil},
		{"ipv6 in listed country", "2001:218:1::1", []string{"JP"}, true, nil},
		{"ipv6 in other country", "2a02:ff00::1", []string{"JP"}, false, nil},
		{"ipv4-mapped ipv6 resolves as ipv4", "::ffff:81.2.69.142", []string{"GB"}, true, nil},
		{"surrounding whitespace tolerated", " 81.2.69.142 ", []string{"GB"}, true, nil},
		{"ip absent from database", "8.8.8.8", []string{"US", "GB"}, false, nil},
		{"invalid ip", "not-an-ip", []string{"US"}, false, iplocationmatch.ErrInvalidIP},
		{"empty ip", "", []string{"US"}, false, iplocationmatch.ErrInvalidIP},
		{"no countries", "81.2.69.142", nil, false, iplocationmatch.ErrNoCountries},
		{"country too long", "81.2.69.142", []string{"USA"}, false, iplocationmatch.ErrInvalidCountry},
		{"country not letters", "81.2.69.142", []string{"U1"}, false, iplocationmatch.ErrInvalidCountry},
		{"one bad country rejects request", "81.2.69.142", []string{"GB", ""}, false, iplocationmatch.ErrInvalidCountry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.InCountries(tt.ip, tt.countries)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := iplocationmatch.Open("/nonexistent/GeoLite2-Country.mmdb"); err == nil {
		t.Fatal("expected error opening missing database")
	}
}

// Run with -race: many goroutines share one Checker.
func TestInCountriesConcurrent(t *testing.T) {
	c := newChecker(t)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			for range 200 {
				ip, country, want := "81.2.69.142", "GB", true
				if i%2 == 1 {
					ip, country, want = "216.160.83.1", "GB", false
				}
				got, err := c.InCountries(ip, []string{country})
				if err != nil || got != want {
					t.Errorf("InCountries(%s,%s) = %v, %v; want %v", ip, country, got, err, want)
					return
				}
			}
		})
	}
	wg.Wait()
}
