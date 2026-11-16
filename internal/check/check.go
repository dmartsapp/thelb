// Package check runs health checks - TCP connect, HTTP GET, or none - and
// turns their results into a state that changes only after a run of them:
// healthy after Rise successes, unhealthy after Fall failures.
package check

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// The defaults.
const (
	Every   = 2 * time.Second
	Timeout = time.Second
	Rise    = 2
	Fall    = 3
)

// Parse checks a check's spec - "tcp", "none" or "http:/path" - for proto, and
// gives the default when spec is empty: tcp for TCP, none otherwise (an SCTP
// check comes later).
func Parse(spec, proto string) (string, error) {
	switch {
	case spec == "":
		if proto == "tcp" {
			return "tcp", nil
		}
		return "none", nil
	case spec == "none":
		return spec, nil
	case spec == "tcp":
		if proto != "tcp" {
			return "", fmt.Errorf("a tcp check is for a tcp offer, not %s", proto)
		}
		return spec, nil
	case strings.HasPrefix(spec, "http:"):
		if proto != "tcp" {
			return "", fmt.Errorf("an http check is for a tcp offer, not %s", proto)
		}
		path := strings.TrimPrefix(spec, "http:")
		if !PathOK(path) {
			return "", fmt.Errorf("an http check's path starts with /, and is up to %d printable ASCII characters, without spaces: %q", maxPath, spec)
		}
		return spec, nil
	}
	return "", fmt.Errorf("a check is tcp, none or http:<path>, not %q", spec)
}

const maxPath = 512

// PathOK is whether path can be an http check's: it begins with "/" and is
// printable ASCII - no space, no control character - up to 512 characters. A
// backend chooses it, and it reaches the leader's requests, status, logs and
// the scripts generate prints.
func PathOK(path string) bool {
	if !strings.HasPrefix(path, "/") || len(path) > maxPath {
		return false
	}
	for i := 0; i < len(path); i++ {
		if path[i] <= ' ' || path[i] >= 0x7f {
			return false
		}
	}
	return true
}

// Describe is a check as status shows it: "tcp", "http /healthz".
func Describe(spec string) string {
	if p, ok := strings.CutPrefix(spec, "http:"); ok {
		return "http " + p
	}
	return spec
}

// Probe runs one check against ap.
func Probe(ctx context.Context, spec string, ap netip.AddrPort, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch {
	case spec == "none":
		return nil
	case spec == "tcp":
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", ap.String())
		if err != nil {
			return err
		}
		return c.Close()
	case strings.HasPrefix(spec, "http:"):
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ap.String()+strings.TrimPrefix(spec, "http:"), nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "theLB health check")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 399 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	}
	return errors.New("an unknown check: " + spec)
}

var client = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // a redirect is an answer
	},
}

// State is a check's state over its results.
type State struct {
	Healthy bool
	Last    error // the last result
	ok      int
	fail    int
}

// Observe takes a result, and reports whether the state changed.
func (s *State) Observe(err error) bool {
	s.Last = err
	if err == nil {
		s.ok, s.fail = s.ok+1, 0
		if !s.Healthy && s.ok >= Rise {
			s.Healthy = true
			return true
		}
		return false
	}
	s.ok, s.fail = 0, s.fail+1
	if s.Healthy && s.fail >= Fall {
		s.Healthy = false
		return true
	}
	return false
}

// Word is the state as status shows it.
func (s *State) Word() string {
	if s.Healthy {
		return "ok"
	}
	return "fail"
}
