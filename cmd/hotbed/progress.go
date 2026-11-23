package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/semistrict/hotbed/internal/api"
)

// progressSeen prints any Hotbed-Progress response header (a cockpit path
// such as "/commands/f-cladefs-g5-9c34da" or a full URL) to stderr once per
// resolved URL, so any RPC that may wait can give callers a live progress
// link without command-specific wiring.
type progressSeen struct {
	base    http.RoundTripper
	cockpit cockpit
	stderr  io.Writer
	mu      *sync.Mutex
	seen    map[string]bool
}

func (p progressSeen) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := p.base.RoundTrip(r)
	if err == nil && resp != nil {
		if raw := strings.TrimSpace(resp.Header.Get(api.ProgressHeader)); raw != "" {
			if target := resolveProgressURL(p.cockpit, raw); target != "" {
				p.mu.Lock()
				already := p.seen[target]
				p.seen[target] = true
				p.mu.Unlock()
				if !already {
					fmt.Fprintf(p.stderr, "hotbed: watch progress at %s\n", target)
				}
			}
		}
	}
	return resp, err
}

func resolveProgressURL(c cockpit, raw string) string {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if c.base == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return c.base + raw
}

// watchProgress wraps d.http and d.stream to surface Hotbed-Progress headers
// from both controller and worker RPC responses on d.stderr.
func watchProgress(d deps) deps {
	if d.stderr == nil {
		return d
	}
	mu := &sync.Mutex{}
	seen := map[string]bool{}
	wrap := func(c *http.Client) *http.Client {
		if c == nil {
			return nil
		}
		base := c.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		w := *c
		w.Transport = progressSeen{
			base:    base,
			cockpit: d.cockpit,
			stderr:  d.stderr,
			mu:      mu,
			seen:    seen,
		}
		return &w
	}
	d.http, d.stream = wrap(d.http), wrap(d.stream)
	return d
}
