package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// MinMinor is the oldest Kubernetes theLB serves: 1.35.
const MinMinor = 35

// requestTimeout bounds every request but a watch.
const requestTimeout = 15 * time.Second

// Client talks to one API server.
type Client struct {
	cfg  *Config
	http *http.Client
}

// New is a client for cfg.
func New(cfg *Config) *Client {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cfg.RootCAs, ServerName: cfg.ServerName}
	if cfg.Cert != nil {
		tc.Certificates = []tls.Certificate{*cfg.Cert}
	}
	tr := &http.Transport{
		TLSClientConfig:       tc,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
		IdleConnTimeout:       90 * time.Second,
		Proxy:                 nil, // straight to the API server: theLB's own traffic never takes a proxy
	}
	return &Client{cfg: cfg, http: &http.Client{Transport: tr}}
}

// Config is the client's configuration.
func (c *Client) Config() *Config { return c.cfg }

// APIError is an answer of the API server other than success.
type APIError struct {
	Code    int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("the API server answered %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("the API server answered %d", e.Code)
}

// IsNotFound is whether err is the API's 404.
func IsNotFound(err error) bool {
	var a *APIError
	return errors.As(err, &a) && a.Code == http.StatusNotFound
}

// IsGone is whether err is the API's 410: a resource version too old to watch
// from, so list again.
func IsGone(err error) bool {
	var a *APIError
	return errors.As(err, &a) && a.Code == http.StatusGone
}

func (c *Client) token() (string, error) {
	if c.cfg.TokenFile != "" {
		b, err := readFile(c.cfg.TokenFile)
		if err != nil {
			return "", fmt.Errorf("the token file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return c.cfg.Token, nil
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body []byte, contentType string) (*http.Response, error) {
	u := *c.cfg.Server
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "thelb")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, apiError(resp.StatusCode, b)
	}
	return resp, nil
}

func apiError(code int, b []byte) error {
	var st status
	if json.Unmarshal(b, &st) == nil && st.Kind == "Status" {
		if st.Code == 0 {
			st.Code = code
		}
		return &APIError{Code: st.Code, Reason: printable(st.Reason), Message: printable(st.Message)}
	}
	msg := strings.TrimSpace(string(b))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &APIError{Code: code, Message: printable(msg)}
}

// printable is what the API server says, fit for a log line or a terminal:
// each control character - a newline, a terminal's escape - a "?".
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// do makes a request and decodes its answer into out, unless out is nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any, contentType string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		if contentType == "" {
			contentType = "application/json"
		}
	}
	resp, err := c.request(ctx, method, path, query, body, contentType)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Version is the API server's version: its minor release, and its own words.
func (c *Client) Version(ctx context.Context) (minor int, git string, err error) {
	var v struct {
		Major      string `json:"major"`
		Minor      string `json:"minor"`
		GitVersion string `json:"gitVersion"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, nil, &v, ""); err != nil {
		return 0, "", err
	}
	m, err := strconv.Atoi(strings.TrimRight(v.Minor, "+"))
	if err != nil || v.Major != "1" {
		return 0, v.GitVersion, fmt.Errorf("the API server's version %q is not one theLB knows", v.GitVersion)
	}
	return m, v.GitVersion, nil
}

// Access is a permission theLB needs: a verb on a resource.
type Access struct {
	Group       string
	Resource    string
	Subresource string
	Verb        string
}

func (a Access) String() string {
	r := a.Resource
	if a.Subresource != "" {
		r += "/" + a.Subresource
	}
	if a.Group != "" {
		r += "." + a.Group
	}
	return a.Verb + " " + r
}

// Allowed asks the API server whether this client may do a: a
// SelfSubjectAccessReview, which every authenticated user may make.
func (c *Client) Allowed(ctx context.Context, a Access) (bool, error) {
	in := map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": map[string]string{
			"group": a.Group, "resource": a.Resource, "subresource": a.Subresource, "verb": a.Verb,
		}},
	}
	var out struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", nil, in, &out, ""); err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

// Get reads the object at path - with query, if any - into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out, "")
}

// Node is the Node named.
func (c *Client) Node(ctx context.Context, name string) (*Node, error) {
	var n Node
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+url.PathEscape(name), nil, nil, &n, ""); err != nil {
		return nil, err
	}
	return &n, nil
}

// SetIngress sets a Service's status.loadBalancer.ingress - the whole list -
// with a merge patch of its status.
func (c *Client) SetIngress(ctx context.Context, namespace, name string, ingress []Ingress) error {
	if ingress == nil {
		ingress = []Ingress{}
	}
	patch := map[string]any{"status": map[string]any{"loadBalancer": map[string]any{"ingress": ingress}}}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/services/" + url.PathEscape(name) + "/status"
	return c.do(ctx, http.MethodPatch, path, nil, patch, nil, "application/merge-patch+json")
}

// Event is an event about a Service, for kubectl describe.
type Event struct {
	Service   Meta
	Warning   bool
	Reason    string // CamelCase: IPAllocated, AllocationFailed, ...
	Action    string
	Note      string
	Instance  string // the director that says it
	Timestamp time.Time
}

// Emit writes an event.
func (c *Client) Emit(ctx context.Context, e Event) error {
	typ := "Normal"
	if e.Warning {
		typ = "Warning"
	}
	note := e.Note
	if len(note) > 1000 {
		note = note[:1000]
	}
	in := map[string]any{
		"apiVersion":          "events.k8s.io/v1",
		"kind":                "Event",
		"metadata":            map[string]string{"generateName": e.Service.Name + ".", "namespace": e.Service.Namespace},
		"eventTime":           e.Timestamp.UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		"reportingController": "dmarts.app/thelb",
		"reportingInstance":   e.Instance,
		"action":              e.Action,
		"reason":              e.Reason,
		"note":                note,
		"type":                typ,
		"regarding": map[string]string{
			"apiVersion": "v1", "kind": "Service", "namespace": e.Service.Namespace, "name": e.Service.Name, "uid": e.Service.UID,
		},
	}
	path := "/apis/events.k8s.io/v1/namespaces/" + url.PathEscape(e.Service.Namespace) + "/events"
	return c.do(ctx, http.MethodPost, path, nil, in, nil, "")
}

// pageSize is how many objects a list asks for at a time.
const pageSize = 500

// List lists every object at path - all of them, page by page - and is the
// resource version to watch from.
func List[T any](ctx context.Context, c *Client, path string) ([]T, string, error) {
	var items []T
	cont := ""
	for {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if cont != "" {
			q.Set("continue", cont)
		}
		var l list[T]
		if err := c.do(ctx, http.MethodGet, path, q, nil, &l, ""); err != nil {
			return nil, "", err
		}
		items = append(items, l.Items...)
		if l.Metadata.Continue == "" {
			return items, l.Metadata.ResourceVersion, nil
		}
		cont = l.Metadata.Continue
	}
}

// Change is one event of a watch.
type Change[T any] struct {
	Type   string // ADDED, MODIFIED, DELETED, BOOKMARK
	Object T
}

// watchTimeout is how long the API server keeps one watch open; the cache
// watches again from where it was.
var watchTimeout = 5 * time.Minute

// Watch watches path from resource version rv, calling fn for each change in
// order, until the watch ends, ctx does, or fn fails. It returns the last
// resource version seen - to watch again from - and why it ended: nil when the
// server closed it; an error IsGone says to list again.
func Watch[T any](ctx context.Context, c *Client, path, rv string, fn func(Change[T]) error) (string, error) {
	q := url.Values{
		"watch":               {"1"},
		"resourceVersion":     {rv},
		"allowWatchBookmarks": {"true"},
		"timeoutSeconds":      {strconv.Itoa(int(watchTimeout / time.Second))},
	}
	resp, err := c.request(ctx, http.MethodGet, path, q, nil, "")
	if err != nil {
		return rv, err
	}
	defer func() { _ = resp.Body.Close() }()
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return rv, ctx.Err()
			}
			return rv, fmt.Errorf("reading the watch: %w", err)
		}
		switch ev.Type {
		case "ERROR":
			var st status
			_ = json.Unmarshal(ev.Object, &st)
			return rv, &APIError{Code: st.Code, Reason: st.Reason, Message: st.Message}
		case "ADDED", "MODIFIED", "DELETED", "BOOKMARK":
		default:
			continue
		}
		var obj T
		if err := json.Unmarshal(ev.Object, &obj); err != nil {
			return rv, fmt.Errorf("reading a watched object: %w", err)
		}
		var m struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if json.Unmarshal(ev.Object, &m) == nil && m.Metadata.ResourceVersion != "" {
			rv = m.Metadata.ResourceVersion
		}
		if ev.Type == "BOOKMARK" {
			continue
		}
		if err := fn(Change[T]{Type: ev.Type, Object: obj}); err != nil {
			return rv, err
		}
	}
}
