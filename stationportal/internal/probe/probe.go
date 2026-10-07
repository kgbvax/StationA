// Package probe checks the landing page's service links from the server side
// (the browser could not: mixed origins, raw TCP, self-signed TLS). A probe
// answers "is something listening and answering" — any HTTP status counts as
// up (a 401 from a digest-protected ESPHome UI is a live UI).
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"stationportal/internal/inventory"
)

// Result is the outcome of the latest probe of one link.
type Result struct {
	Kind    string // "http", "tcp", "none"
	Up      bool
	Detail  string
	Latency time.Duration
	At      time.Time
}

// Prober probes links periodically and keeps the latest result per URL.
type Prober struct {
	links   []inventory.Link
	timeout time.Duration
	http    *http.Client
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)

	mu      sync.RWMutex
	results map[string]Result
}

// New returns a prober for links with a per-probe timeout.
func New(links []inventory.Link, timeout time.Duration) *Prober {
	tr := &http.Transport{
		// Reachability only: the UniFi console and similar LAN UIs use
		// self-signed certificates. Nothing is sent but a bare GET.
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // reachability probe only
		DisableKeepAlives: true,
	}
	d := &net.Dialer{}
	return &Prober{
		links:   links,
		timeout: timeout,
		http: &http.Client{
			Transport: tr,
			// Do not follow redirects: a 30x already proves the UI answers.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		dial:    d.DialContext,
		results: map[string]Result{},
	}
}

// Run probes all links now and then every interval until ctx is done.
func (p *Prober) Run(ctx context.Context, interval time.Duration) {
	p.ProbeAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.ProbeAll(ctx)
		}
	}
}

// ProbeAll probes every link concurrently and stores the results.
func (p *Prober) ProbeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, l := range p.links {
		wg.Add(1)
		go func(l inventory.Link) {
			defer wg.Done()
			r := p.probe(ctx, l)
			p.mu.Lock()
			p.results[l.URL] = r
			p.mu.Unlock()
		}(l)
	}
	wg.Wait()
}

// Result returns the latest result for a link URL (zero Result if none yet).
func (p *Prober) Result(url string) (Result, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.results[url]
	return r, ok
}

func (p *Prober) probe(ctx context.Context, l inventory.Link) Result {
	kind, target, err := inventory.ProbeTarget(l)
	start := time.Now()
	r := Result{Kind: kind, At: start}
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	switch kind {
	case "none":
		r.Detail = "not probed"
		return r
	case "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			r.Detail = err.Error()
			return r
		}
		resp, err := p.http.Do(req)
		if err != nil {
			r.Detail = shortErr(err)
			return r
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		r.Up, r.Detail = true, fmt.Sprintf("HTTP %d", resp.StatusCode)
	case "tcp":
		c, err := p.dial(ctx, "tcp", target)
		if err != nil {
			r.Detail = shortErr(err)
			return r
		}
		c.Close()
		r.Up, r.Detail = true, "TCP open"
	}
	r.Latency = time.Since(start)
	return r
}

// shortErr trims Go's nested net errors to their cause for display.
func shortErr(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}
