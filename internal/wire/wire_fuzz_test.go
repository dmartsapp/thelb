package wire

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// byteConn is a connection that reads from a buffer and writes nowhere.
type byteConn struct{ r *bytes.Reader }

func (c byteConn) Read(b []byte) (int, error)       { return c.r.Read(b) }
func (c byteConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c byteConn) Close() error                     { return nil }
func (c byteConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c byteConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c byteConn) SetDeadline(time.Time) error      { return nil }
func (c byteConn) SetReadDeadline(time.Time) error  { return nil }
func (c byteConn) SetWriteDeadline(time.Time) error { return nil }

// FuzzRecv: the bytes a peer sends - before any proof, on a join. Recv reads
// one line at most MaxLine long, and gives back what JSON makes of it or an
// error; it never reads past the line, and never holds more than MaxLine.
func FuzzRecv(f *testing.F) {
	f.Add([]byte(`{"v":1,"role":"backend","proof":"AAAA"}` + "\n"))
	f.Add([]byte(`{"v":1}` + "\n" + `{"v":2}` + "\n"))
	f.Add([]byte(`[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[`))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(`{"v":1,"claim":{"name":"` + string(make([]byte, 70000)) + `"}}` + "\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := NewConn(byteConn{bytes.NewReader(b)})
		for i := 0; i < 4; i++ {
			var v map[string]any
			err := c.Recv(&v)
			if err != nil {
				return
			}
			if _, err := json.Marshal(v); err != nil {
				t.Fatalf("decoded something JSON cannot write: %v", err)
			}
		}
	})
}

// TestRecvRefusesALineLongerThanMaxLine, whole or across reads.
func TestRecvRefusesALineLongerThanMaxLine(t *testing.T) {
	long := append(bytes.Repeat([]byte("a"), MaxLine+1), '\n')
	var v any
	if err := NewConn(byteConn{bytes.NewReader(long)}).Recv(&v); err == nil || err.Error() != "a message is too long" {
		t.Fatalf("%v", err)
	}
	endless := bytes.Repeat([]byte("{"), 3*MaxLine) // no newline at all
	if err := NewConn(byteConn{bytes.NewReader(endless)}).Recv(&v); err == nil {
		t.Fatal("a line without an end was read")
	}
}
