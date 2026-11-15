// Package kube is the little of the Kubernetes API theLB uses, on Go's
// standard library: a kubeconfig or a pod's ServiceAccount, lists and watches
// kept as caches, a Service's status, access reviews and events. theLB keeps
// to two dependencies; client-go would bring dozens.
package kube

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// InCluster is what --kubeconfig says for a director in a pod: the pod's own
// ServiceAccount.
const InCluster = "in-cluster"

// Where a pod finds its ServiceAccount. Variables, for tests.
var (
	saDir    = "/var/run/secrets/kubernetes.io/serviceaccount"
	getenv   = os.Getenv
	readFile = os.ReadFile
)

// Config is how to reach the API server, and who to be there.
type Config struct {
	Server     *url.URL
	RootCAs    *x509.CertPool   // nil: the system's
	ServerName string           // the name the server's certificate is checked for; "": the server's host
	Cert       *tls.Certificate // a client certificate, or nil
	Token      string           // a bearer token, or ""
	TokenFile  string           // a file holding one, read for each request - a pod's rotates
	Namespace  string           // the context's, or the pod's
	Source     string           // where it came from, for messages
}

// Load reads a kubeconfig file, or - for InCluster - the pod's ServiceAccount.
func Load(path string) (*Config, error) {
	if path == InCluster {
		return inCluster()
	}
	b, err := readFile(path)
	if err != nil {
		return nil, err
	}
	c, err := parseKubeconfig(b, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig %s: %w", path, err)
	}
	c.Source = path
	return c, nil
}

func inCluster() (*Config, error) {
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("--kubeconfig in-cluster: not in a pod - KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	u, err := url.Parse("https://" + net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	ca, err := readFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("--kubeconfig in-cluster: the pod's ServiceAccount: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("--kubeconfig in-cluster: the ServiceAccount's ca.crt holds no certificate")
	}
	tokenFile := filepath.Join(saDir, "token")
	if _, err := readFile(tokenFile); err != nil {
		return nil, fmt.Errorf("--kubeconfig in-cluster: the pod's ServiceAccount: %w", err)
	}
	ns, _ := readFile(filepath.Join(saDir, "namespace"))
	return &Config{Server: u, RootCAs: pool, TokenFile: tokenFile, Namespace: strings.TrimSpace(string(ns)), Source: "the pod's ServiceAccount"}, nil
}

// parseKubeconfig reads a kubeconfig's current context: its cluster and user.
// Files it names are relative to dir.
func parseKubeconfig(b []byte, dir string) (*Config, error) {
	doc, err := decodeYAML(b)
	if err != nil {
		return nil, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("not a kubeconfig: no mapping at its top")
	}
	current := str(root["current-context"])
	if current == "" {
		return nil, errors.New("no current-context")
	}
	ctx := named(root["contexts"], current, "context")
	if ctx == nil {
		return nil, fmt.Errorf("no context named %q", current)
	}
	clusterName, userName := str(ctx["cluster"]), str(ctx["user"])
	cl := named(root["clusters"], clusterName, "cluster")
	if cl == nil {
		return nil, fmt.Errorf("the context %q names the cluster %q, which it does not have", current, clusterName)
	}
	c := &Config{Namespace: str(ctx["namespace"])}
	server := str(cl["server"])
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("the cluster %q's server %q is not a URL", clusterName, server)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("the cluster %q's server %q: theLB talks to the API server over HTTPS only", clusterName, server)
	}
	c.Server = u
	if str(cl["insecure-skip-tls-verify"]) == "true" {
		return nil, fmt.Errorf("the cluster %q sets insecure-skip-tls-verify: theLB talks only to an API server it can verify - give its CA", clusterName)
	}
	c.ServerName = str(cl["tls-server-name"])
	var caPEM []byte
	switch {
	case str(cl["certificate-authority-data"]) != "":
		if caPEM, err = base64.StdEncoding.DecodeString(str(cl["certificate-authority-data"])); err != nil {
			return nil, fmt.Errorf("the cluster %q's certificate-authority-data: %w", clusterName, err)
		}
	case str(cl["certificate-authority"]) != "":
		if caPEM, err = readFile(rel(dir, str(cl["certificate-authority"]))); err != nil {
			return nil, fmt.Errorf("the cluster %q's certificate-authority: %w", clusterName, err)
		}
	}
	if caPEM != nil {
		c.RootCAs = x509.NewCertPool()
		if !c.RootCAs.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("the cluster %q's certificate authority holds no certificate", clusterName)
		}
	}
	us := named(root["users"], userName, "user")
	if us == nil {
		return nil, fmt.Errorf("the context %q names the user %q, which it does not have", current, userName)
	}
	for _, unsupported := range []string{"exec", "auth-provider", "username", "password"} {
		if us[unsupported] != nil {
			return nil, fmt.Errorf("the user %q authenticates with %s, which theLB does not do: give it a ServiceAccount token - thelb generate k8s prints one", userName, unsupported)
		}
	}
	c.Token = str(us["token"])
	if f := str(us["tokenFile"]); f != "" && c.Token == "" {
		c.TokenFile = rel(dir, f)
		if _, err := readFile(c.TokenFile); err != nil {
			return nil, fmt.Errorf("the user %q's tokenFile: %w", userName, err)
		}
	}
	certPEM, keyPEM := []byte(nil), []byte(nil)
	if d := str(us["client-certificate-data"]); d != "" {
		if certPEM, err = base64.StdEncoding.DecodeString(d); err != nil {
			return nil, fmt.Errorf("the user %q's client-certificate-data: %w", userName, err)
		}
	} else if f := str(us["client-certificate"]); f != "" {
		if certPEM, err = readFile(rel(dir, f)); err != nil {
			return nil, fmt.Errorf("the user %q's client-certificate: %w", userName, err)
		}
	}
	if d := str(us["client-key-data"]); d != "" {
		if keyPEM, err = base64.StdEncoding.DecodeString(d); err != nil {
			return nil, fmt.Errorf("the user %q's client-key-data: %w", userName, err)
		}
	} else if f := str(us["client-key"]); f != "" {
		if keyPEM, err = readFile(rel(dir, f)); err != nil {
			return nil, fmt.Errorf("the user %q's client-key: %w", userName, err)
		}
	}
	if (certPEM == nil) != (keyPEM == nil) {
		return nil, fmt.Errorf("the user %q has a client certificate without its key, or a key without its certificate", userName)
	}
	if certPEM != nil {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("the user %q's client certificate: %w", userName, err)
		}
		c.Cert = &cert
	}
	if c.Token == "" && c.TokenFile == "" && c.Cert == nil {
		return nil, fmt.Errorf("the user %q has no credential: a token, a tokenFile, or a client certificate", userName)
	}
	return c, nil
}

// named is the entry of a kubeconfig list - contexts, clusters, users - with
// that name: its inner mapping (field).
func named(list any, name, field string) map[string]any {
	items, _ := list.([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m != nil && str(m["name"]) == name {
			inner, _ := m[field].(map[string]any)
			if inner == nil {
				inner = map[string]any{}
			}
			return inner
		}
	}
	return nil
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

func rel(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}
