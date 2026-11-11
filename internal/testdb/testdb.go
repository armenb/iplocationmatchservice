// Package testdb builds small GeoLite2-Country-shaped .mmdb files for tests,
// so the suite needs neither the licensed MaxMind database nor network access.
package testdb

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// NoCountry marks an entry whose record has no "country" key, only a
// "registered_country" (US). Some real GeoLite2 networks look like this.
const NoCountry = ""

// Fixture is the CIDR -> ISO country code mapping used across the test suite.
var Fixture = map[string]string{
	"81.2.69.0/24":    "GB",
	"216.160.83.0/24": "US",
	"89.160.20.0/24":  "SE",
	"2001:218::/32":   "JP",
	"2a02:ff00::/32":  "DE",
	"203.0.113.0/24":  NoCountry,
}

// Write creates a GeoLite2-Country-style database containing entries in a
// temporary directory and returns its path.
func Write(tb testing.TB, entries map[string]string) string {
	tb.Helper()

	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "GeoLite2-Country",
		IPVersion:               6,
		RecordSize:              28,
		IncludeReservedNetworks: true,
	})
	if err != nil {
		tb.Fatalf("mmdbwriter.New: %v", err)
	}
	for cidr, code := range entries {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			tb.Fatalf("parse CIDR %q: %v", cidr, err)
		}
		rec := mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(code)}}
		if code == NoCountry {
			rec = mmdbtype.Map{"registered_country": mmdbtype.Map{"iso_code": mmdbtype.String("US")}}
		}
		if err := w.Insert(n, rec); err != nil {
			tb.Fatalf("insert %s: %v", cidr, err)
		}
	}

	path := filepath.Join(tb.TempDir(), "GeoLite2-Country.mmdb")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("create %s: %v", path, err)
	}
	if _, err := w.WriteTo(f); err != nil {
		_ = f.Close()
		tb.Fatalf("write mmdb: %v", err)
	}
	if err := f.Close(); err != nil { // a failed close can mean unflushed data
		tb.Fatalf("close %s: %v", path, err)
	}
	return path
}
