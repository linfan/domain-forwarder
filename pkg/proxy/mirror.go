package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"domain-forwarder/pkg/forwarder"
)

// mirrorWorkers bounds how many mirror deliveries run concurrently.
const mirrorWorkers = 4

// MirrorConfig configures best-effort traffic mirroring.
type MirrorConfig struct {
	Target       *url.URL      // Base URL; the original path and query are appended
	Timeout      time.Duration // Per-delivery timeout
	MaxBodyBytes int64         // Max request body captured; larger bodies are not mirrored
	QueueSize    int           // Pending jobs; overflow is dropped
	Debug        bool
}

// Mirror delivers an asynchronous copy of every plain-HTTP proxy request to a
// secondary HTTP target. Delivery is best-effort: the queue is bounded and
// every failure (overflow, dial error, timeout) is dropped silently unless
// debug is on. CONNECT tunnels are not mirrored, since their content is an
// opaque TLS stream.
type Mirror struct {
	target  *url.URL
	timeout time.Duration
	maxBody int64
	debug   bool
	jobs    chan mirrorJob
	client  *http.Client
}

// mirrorJob is one snapshotted request waiting for delivery.
type mirrorJob struct {
	method string
	url    string
	header http.Header
	body   []byte
}

// NewMirror creates the mirror queue and its delivery workers.
func NewMirror(config MirrorConfig) *Mirror {
	m := &Mirror{
		target:  config.Target,
		timeout: config.Timeout,
		maxBody: config.MaxBodyBytes,
		debug:   config.Debug,
		jobs:    make(chan mirrorJob, config.QueueSize),
	}
	m.client = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		// The mirror response is discarded; following redirects adds nothing.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for i := 0; i < mirrorWorkers; i++ {
		go m.worker()
	}
	return m
}

// prepare snapshots r for mirroring and replaces r.Body with a capture wrapper
// so the body is copied while the upstream consumes it. A request without a
// body is enqueued immediately; a request with a body is enqueued once the
// upstream reaches EOF. Bodies larger than maxBody (or bodies the upstream never
// fully reads) are not mirrored.
func (m *Mirror) prepare(r *http.Request) {
	if r.ContentLength > m.maxBody {
		m.debugf("body of %s %s is %d bytes (limit %d), not mirrored",
			r.Method, r.URL, r.ContentLength, m.maxBody)
		return
	}

	job := mirrorJob{
		method: r.Method,
		url:    m.mirrorURL(r.URL),
		header: mirrorHeaders(r),
	}
	if r.Body == nil || r.Body == http.NoBody {
		m.enqueue(job)
		return
	}

	r.Body = &captureBody{
		src:   r.Body,
		limit: m.maxBody,
		done: func(body []byte, complete bool) {
			if !complete {
				m.debugf("body of %s %s was not fully captured, not mirrored", job.method, job.url)
				return
			}
			job.body = body
			m.enqueue(job)
		},
	}
}

// mirrorURL joins the mirror target's scheme, host and base path with the
// original path and query, preserving the original percent-encoding.
func (m *Mirror) mirrorURL(original *url.URL) string {
	base := strings.TrimSuffix(m.target.String(), "/")
	path := original.EscapedPath()
	if path == "" {
		path = "/"
	}
	if original.RawQuery != "" {
		return base + path + "?" + original.RawQuery
	}
	return base + path
}

// mirrorHeaders snapshots the headers that would be forwarded upstream: hop-by-
// hop headers are dropped, the Host is rewritten to the mirror target, and the
// original Host is carried in X-Forwarded-Host when it is not already set.
func mirrorHeaders(r *http.Request) http.Header {
	h := r.Header.Clone()
	forwarder.RemoveHopByHopHeaders(h)
	if h.Get("X-Forwarded-Host") == "" {
		h.Set("X-Forwarded-Host", r.Host)
	}
	return h
}

func (m *Mirror) enqueue(job mirrorJob) {
	select {
	case m.jobs <- job:
	default:
		m.debugf("queue full, dropped %s %s", job.method, job.url)
	}
}

func (m *Mirror) worker() {
	for job := range m.jobs {
		m.deliver(job)
	}
}

func (m *Mirror) deliver(job mirrorJob) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, job.method, job.url, bytes.NewReader(job.body))
	if err != nil {
		m.debugf("build request for %s %s: %v", job.method, job.url, err)
		return
	}
	req.Header = job.header
	req.ContentLength = int64(len(job.body))

	resp, err := m.client.Do(req)
	if err != nil {
		m.debugf("deliver %s %s: %v", job.method, job.url, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	m.debugf("delivered %s %s status=%d", job.method, job.url, resp.StatusCode)
}

func (m *Mirror) debugf(format string, args ...any) {
	if m.debug {
		log.Printf("[MIRROR] "+format, args...)
	}
}

// captureBody tees the upstream request body into memory while the transport
// reads it, so a mirror copy is enqueued only once the whole body is known.
// Read and Close may race (the transport closes the body on cancellation), so
// the buffer state is mutex-protected.
type captureBody struct {
	src   io.ReadCloser
	limit int64
	done  func(body []byte, complete bool)

	mu       sync.Mutex
	buf      []byte
	over     bool
	finished bool
}

func (c *captureBody) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)

	c.mu.Lock()
	if n > 0 && !c.over && !c.finished {
		if int64(len(c.buf))+int64(n) > c.limit {
			c.over = true
			c.buf = nil
		} else {
			c.buf = append(c.buf, p[:n]...)
		}
	}
	c.mu.Unlock()

	if err == io.EOF {
		c.finish(true)
	} else if err != nil {
		c.finish(false)
	}
	return n, err
}

func (c *captureBody) Close() error {
	c.finish(false)
	return c.src.Close()
}

// finish publishes the captured body exactly once. complete reports whether the
// source reached EOF before the body was closed.
func (c *captureBody) finish(complete bool) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	body, over := c.buf, c.over
	c.mu.Unlock()

	c.done(body, complete && !over)
}
