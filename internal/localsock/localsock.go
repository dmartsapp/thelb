// Package localsock is how `thelb status`, `doctor` and `admin` ask the theLB
// running on this node: a Unix socket in /run/thelb, one JSON request and one
// JSON answer per connection. Only root may ask - the kernel tells the server
// who is calling - and only what each command shows crosses it.
package localsock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/status"
)

// Names in the run directory, and the protocol's version.
const (
	SocketName = "thelb.sock"
	lockName   = "thelb.lock"
	Version    = 1

	maxRequest = 64 << 10
	maxAnswer  = 16 << 20
	deadline   = 15 * time.Second // an admin change waits for the leader
)

// The answers a caller can act on.
var (
	ErrAlreadyRunning = errors.New("theLB is already running on this node")
	ErrNotRunning     = errors.New("theLB is not running on this node")
)

// Operations.
const (
	OpStatus     = "status"
	OpSecretShow = "secret.show"
	OpApprove    = "approve"
	OpRefuse     = "refuse"
	OpRemove     = "remove"
)

// Request is what a command asks.
type Request struct {
	V     int      `json:"v"`
	Op    string   `json:"op"`
	Role  string   `json:"role,omitempty"`
	Names []string `json:"names,omitempty"`
}

// Response is the answer.
type Response struct {
	V      int            `json:"v"`
	OK     bool           `json:"ok"`
	Error  string         `json:"error,omitempty"`
	Status *status.Status `json:"status,omitempty"`
	Secret string         `json:"secret,omitempty"`
	Names  []string       `json:"names,omitempty"`
}

// Handler answers requests: the running director or backend.
type Handler interface {
	Status() status.Status
	SecretText(role string) (string, error)
	// Admin approves, refuses or removes nodes, and returns the names changed.
	Admin(op string, names []string) ([]string, error)
}

// Server is the listening end.
type Server struct {
	dir  string
	h    Handler
	log  *logx.Logger
	ln   *net.UnixListener
	lock *os.File
	wg   sync.WaitGroup
	once sync.Once

	// PeerUID returns the calling process's effective user ID; replaced in
	// tests, which cannot call as root.
	PeerUID func(*net.UnixConn) (int, error)
}

// Listen takes the run directory for this process - one theLB per node - and
// listens on its socket. A socket left by a crash is replaced.
func Listen(dir string, h Handler, log *logx.Logger) (*Server, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the lock in %s: %w", dir, err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	sock := filepath.Join(dir, SocketName)
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			unlock(lock)
			return nil, fmt.Errorf("%s exists and is not a socket", sock)
		}
		// We hold the lock, so nothing listens on it: a crash left it.
		if err := os.Remove(sock); err != nil {
			unlock(lock)
			return nil, fmt.Errorf("removing the stale %s: %w", sock, err)
		}
		log.Warn("stale socket replaced", "socket", sock)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		unlock(lock)
		return nil, fmt.Errorf("listening on %s: %w", sock, err)
	}
	ln.SetUnlinkOnClose(true)
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		unlock(lock)
		return nil, fmt.Errorf("%s: %w", sock, err)
	}
	return &Server{dir: dir, h: h, log: log, ln: ln, lock: lock, PeerUID: peerUID}, nil
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// Path is the socket's path.
func (s *Server) Path() string { return filepath.Join(s.dir, SocketName) }

// Serve answers requests until ctx is done.
func (s *Server) Serve(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { _ = s.ln.Close() })
	defer stop()
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			s.log.Warn("accepting a connection failed", "error", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
	s.wg.Wait()
}

// Close stops listening, removes the socket and the lock, and the run directory
// if nothing else is in it. It is safe to call more than once.
func (s *Server) Close() {
	s.once.Do(func() {
		_ = s.ln.Close()
		s.wg.Wait()
		_ = os.Remove(filepath.Join(s.dir, lockName))
		unlock(s.lock)
		_ = os.Remove(s.dir) // only if empty
	})
}

func (s *Server) handle(c *net.UnixConn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(deadline))
	answer := func(r Response) {
		r.V = Version
		_ = json.NewEncoder(c).Encode(r)
	}
	uid, err := s.PeerUID(c)
	if err != nil || uid != 0 {
		s.log.Warn("request refused: the caller is not root", "uid", uid, "error", err)
		answer(Response{Error: "only root may ask theLB"})
		return
	}
	line, err := bufio.NewReader(io.LimitReader(c, maxRequest)).ReadBytes('\n')
	if err != nil {
		answer(Response{Error: "the request did not arrive whole"})
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		answer(Response{Error: "the request is not JSON"})
		return
	}
	if req.V != Version {
		answer(Response{Error: fmt.Sprintf("this theLB speaks version %d of the local protocol, not %d", Version, req.V)})
		return
	}
	switch req.Op {
	case OpStatus:
		st := s.h.Status()
		st.Normalize()
		answer(Response{OK: true, Status: &st})
	case OpSecretShow:
		text, err := s.h.SecretText(req.Role)
		if err != nil {
			answer(Response{Error: err.Error()})
			return
		}
		s.log.OK("secret shown", "role", req.Role)
		answer(Response{OK: true, Secret: text})
	case OpApprove, OpRefuse, OpRemove:
		names, err := s.h.Admin(req.Op, req.Names)
		if err != nil {
			answer(Response{Error: err.Error()})
			return
		}
		answer(Response{OK: true, Names: names})
	default:
		answer(Response{Error: fmt.Sprintf("this theLB does not know %q", req.Op)})
	}
}

// Call asks the theLB running on this node, whose run directory is dir.
func Call(dir string, req Request) (Response, error) {
	c, err := net.DialTimeout("unix", filepath.Join(dir, SocketName), deadline)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			return Response{}, ErrNotRunning
		}
		return Response{}, fmt.Errorf("asking theLB: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(deadline))
	req.V = Version
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, fmt.Errorf("asking theLB: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(io.LimitReader(c, maxAnswer)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading theLB's answer: %w", err)
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
