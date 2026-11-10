//go:build !linux

package ctlnet

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/logx"
)

// Net is a director's network; directors run only on Linux.
type Net struct{}

// New fails: directors run only on Linux.
func New(netip.Addr, uint16, hostnet.Address, *logx.Logger) (*Net, error) {
	return nil, errors.New("a director's network is Linux only")
}

func (*Net) ListenNode() (net.Listener, error)              { return nil, errors.ErrUnsupported }
func (*Net) DialVIP(context.Context) (net.Conn, error)      { return nil, errors.ErrUnsupported }
func (*Net) Dial(context.Context, string) (net.Conn, error) { return nil, errors.ErrUnsupported }
func (*Net) VIPHolder(context.Context) (string, error)      { return "", errors.ErrUnsupported }
func (*Net) Take() (net.Listener, error)                    { return nil, errors.ErrUnsupported }
func (*Net) Release()                                       {}
func (*Net) Repair() (bool, error)                          { return false, errors.ErrUnsupported }
func (*Net) Cleanup() error                                 { return errors.ErrUnsupported }
func (*Net) MACOf(netip.Addr) string                        { return "" }
func (*Net) Holder(context.Context, netip.Addr) (string, error) {
	return "", errors.ErrUnsupported
}
func (*Net) Uplink(context.Context) (bool, string) {
	return false, "a director's network is Linux only"
}
