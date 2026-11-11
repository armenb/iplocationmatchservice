// Package statsd is a minimal, dependency-free DogStatsD client: counters and
// timers over UDP, with tags.
//
// Plain statsd has no tags; DogStatsD extends the line protocol with
// "|#key:value,key:value", which Datadog, Telegraf and statsd_exporter accept.
//
// The client never blocks or fails a caller. Metrics are queued to a single
// writer goroutine that batches them into datagrams; if the queue is full or the
// agent is unreachable they are dropped and counted (see Dropped).
package statsd

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxPacket is the datagram payload size DogStatsD recommends for UDP: it
	// fits an Ethernet MTU without fragmentation.
	maxPacket     = 1432
	queueSize     = 4096
	flushInterval = 100 * time.Millisecond
)

// Client sends metrics to a DogStatsD/statsd agent. It is safe for concurrent use.
type Client struct {
	conn    net.Conn
	tags    string // constant tags, comma-joined, applied to every metric
	lines   chan []byte
	quit    chan struct{}
	done    chan struct{}
	dropped atomic.Uint64

	closeOnce sync.Once
	closeErr  error
}

// New creates a client that sends to the UDP address addr ("host:port").
// constTags ("key:value") are added to every metric. UDP is connectionless, so
// New succeeds even if no agent is listening yet.
func New(addr string, constTags ...string) (*Client, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:  conn,
		tags:  sanitize(strings.Join(constTags, ",")),
		lines: make(chan []byte, queueSize),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go c.run()
	return c, nil
}

// Incr adds 1 to the counter name. tags are "key:value" pairs; callers pass
// only low-cardinality values (the metric backend bills per distinct tag set).
func (c *Client) Incr(name string, tags ...string) {
	c.enqueue(name, "1", "c", tags)
}

// Timing records a duration for name, in milliseconds.
func (c *Client) Timing(name string, d time.Duration, tags ...string) {
	c.enqueue(name, strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64), "ms", tags)
}

// Dropped reports how many metrics were discarded because the queue was full.
func (c *Client) Dropped() uint64 { return c.dropped.Load() }

// Close flushes queued metrics and releases the socket. It is safe to call from
// several goroutines and more than once: every call waits for the first to
// finish and returns its result. Calls to Incr and Timing after Close are safe
// and are dropped.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.quit)
		<-c.done
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

func (c *Client) enqueue(name, value, kind string, tags []string) {
	line := make([]byte, 0, 96)
	line = append(line, name...)
	line = append(line, ':')
	line = append(line, value...)
	line = append(line, '|')
	line = append(line, kind...)
	if c.tags != "" || len(tags) > 0 {
		line = append(line, "|#"...)
		line = append(line, c.tags...)
		for i, t := range tags {
			if i > 0 || c.tags != "" {
				line = append(line, ',')
			}
			line = append(line, t...)
		}
	}
	select {
	case c.lines <- line:
	default:
		c.dropped.Add(1)
	}
}

// run batches queued lines, newline-separated, into datagrams of at most
// maxPacket bytes, flushing every flushInterval and on Close.
func (c *Client) run() {
	defer close(c.done)
	tick := time.NewTicker(flushInterval)
	defer tick.Stop()

	buf := make([]byte, 0, maxPacket)
	flush := func() {
		if len(buf) > 0 {
			_, _ = c.conn.Write(buf) // best effort: a missing agent must not affect requests
			buf = buf[:0]
		}
	}
	add := func(line []byte) {
		if len(buf) > 0 && len(buf)+1+len(line) > maxPacket {
			flush()
		}
		if len(buf) > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, line...)
	}
	for {
		select {
		case line := <-c.lines:
			add(line)
		case <-tick.C:
			flush()
		case <-c.quit:
			for {
				select {
				case line := <-c.lines:
					add(line)
				default:
					flush()
					return
				}
			}
		}
	}
}

// sanitize removes characters that would corrupt the line protocol.
func sanitize(s string) string {
	return strings.NewReplacer("|", "_", "\n", "_", "\r", "_").Replace(s)
}
