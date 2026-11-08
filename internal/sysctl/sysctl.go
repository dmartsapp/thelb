// Package sysctl sets kernel parameters at runtime - writing /proc/sys, as the
// sysctl command does - and records what it changed, so it can be put back. A
// value that is already right is the user's, and left alone.
package sysctl

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Root is where the parameters are; tests replace it.
var Root = "/proc/sys"

// Read is a parameter's value: "net/ipv4/conf/all/arp_ignore".
func Read(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(Root, name))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Write sets a parameter.
func Write(name, value string) error {
	if err := os.WriteFile(filepath.Join(Root, name), []byte(value+"\n"), 0o644); err != nil {
		return fmt.Errorf("setting %s to %s: %w", name, value, err)
	}
	return nil
}

// Changes are the parameters theLB changed, with their old values.
type Changes struct {
	mu    sync.Mutex
	old   map[string]string
	order []string
}

// Ensure sets name to want unless its value is already one of fine (want, if
// fine is empty). It returns the old value when it changed it.
func (c *Changes) Ensure(name, want string, fine ...string) (old string, changed bool, err error) {
	cur, err := Read(name)
	if err != nil {
		return "", false, err
	}
	if len(fine) == 0 {
		fine = []string{want}
	}
	if slices.Contains(fine, cur) {
		return cur, false, nil
	}
	if err := Write(name, want); err != nil {
		return cur, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.old == nil {
		c.old = map[string]string{}
	}
	if _, ok := c.old[name]; !ok {
		c.old[name] = cur
		c.order = append(c.order, name)
	}
	return cur, true, nil
}

// Restored is one parameter put back.
type Restored struct {
	Name, Value string
	Err         error
}

// Restore puts every changed parameter back, last changed first.
func (c *Changes) Restore() []Restored {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Restored
	for i := len(c.order) - 1; i >= 0; i-- {
		name := c.order[i]
		out = append(out, Restored{Name: name, Value: c.old[name], Err: Write(name, c.old[name])})
	}
	c.old, c.order = nil, nil
	return out
}
