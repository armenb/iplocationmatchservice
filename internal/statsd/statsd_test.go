package statsd_test

import (
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"iplocationmatchservice/internal/statsd"
)

// listen returns a UDP socket standing in for a statsd agent.
func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readLines collects datagrams until the socket is quiet, returning each
// datagram's lines and the total size of the largest datagram.
func readLines(t *testing.T, conn *net.UDPConn) (lines []string, maxDatagram int) {
	t.Helper()
	buf := make([]byte, 65535)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return lines, maxDatagram
		}
		maxDatagram = max(maxDatagram, n)
		lines = append(lines, strings.Split(string(buf[:n]), "\n")...)
	}
}

func TestWireFormat(t *testing.T) {
	agent := listen(t)
	c, err := statsd.New(agent.LocalAddr().String(), "db_version:2026-11-10")
	if err != nil {
		t.Fatal(err)
	}
	c.Incr("iplocationmatch.requests", "transport:http", "result:match")
	c.Timing("iplocationmatch.request.duration", 1500*time.Microsecond, "transport:http")
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, _ := readLines(t, agent)
	want := []string{
		"iplocationmatch.requests:1|c|#db_version:2026-11-10,transport:http,result:match",
		"iplocationmatch.request.duration:1.500|ms|#db_version:2026-11-10,transport:http",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestNoTagsOmitsTagSection(t *testing.T) {
	agent := listen(t)
	c, err := statsd.New(agent.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Incr("x")
	_ = c.Close()

	got, _ := readLines(t, agent)
	if !slices.Equal(got, []string{"x:1|c"}) {
		t.Fatalf("lines = %q", got)
	}
}

func TestConstTagsAreSanitized(t *testing.T) {
	agent := listen(t)
	c, err := statsd.New(agent.LocalAddr().String(), "v:a|b\nc")
	if err != nil {
		t.Fatal(err)
	}
	c.Incr("x")
	_ = c.Close()

	got, _ := readLines(t, agent)
	if !slices.Equal(got, []string{"x:1|c|#v:a_b_c"}) {
		t.Fatalf("lines = %q", got)
	}
}

// Many metrics are batched into datagrams that stay under the UDP-safe size.
func TestBatchingRespectsPacketSize(t *testing.T) {
	agent := listen(t)
	c, err := statsd.New(agent.LocalAddr().String(), "db_version:2026-11-10")
	if err != nil {
		t.Fatal(err)
	}
	for range 400 {
		c.Incr("iplocationmatch.requests", "transport:grpc", "result:no_match")
	}
	_ = c.Close()

	lines, maxDatagram := readLines(t, agent)
	if len(lines) < 2 {
		t.Fatalf("got %d lines, expected batching across datagrams", len(lines))
	}
	if maxDatagram > 1432 {
		t.Fatalf("largest datagram %d bytes, want <= 1432", maxDatagram)
	}
}

// A request must never block or panic because of metrics, even after Close or
// when the queue overflows.
func TestNeverBlocksOrPanics(t *testing.T) {
	agent := listen(t)
	c, err := statsd.New(agent.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	_ = c.Close() // idempotent

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range 20000 { // far more than the queue holds, with no writer draining
			c.Incr("x")
			c.Timing("y", time.Millisecond)
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Incr/Timing blocked after Close")
	}
	if c.Dropped() == 0 {
		t.Fatal("expected overflow to be counted as dropped")
	}
}

// Close may be called from several goroutines at once (a deferred Close racing a
// shutdown path, say). It must neither panic nor report different results.
func TestConcurrentCloseIsSafe(t *testing.T) {
	for range 200 {
		agent := listen(t)
		c, err := statsd.New(agent.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		errs := make(chan error, 8)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { errs <- c.Close() })
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		}
	}
}
