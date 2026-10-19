//go:build !linux

package fastpath

import (
	"errors"
	"net/netip"
	"time"

	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

var errNotLinux = errors.New("the eBPF fast path needs Linux")

// FastPath is not available off Linux: Open always fails.
type FastPath struct{}

func Open(Options) (*FastPath, error) { return nil, errNotLinux }

func Detach(string) error { return errNotLinux }

func (*FastPath) SetEntries([]netip.AddrPort) error               { return errNotLinux }
func (*FastPath) Entries() ([]netip.AddrPort, error)              { return nil, errNotLinux }
func (*FastPath) Upsert(demux.Forward) error                      { return errNotLinux }
func (*FastPath) Delete(uint32) error                             { return errNotLinux }
func (*FastPath) TowardsServer(demux.Socket) (demux.Socket, bool) { return demux.Socket{}, false }
func (*FastPath) TowardsClient(demux.Socket) (demux.Socket, bool) { return demux.Socket{}, false }
func (*FastPath) LastSeen() (map[uint32]time.Time, error)         { return nil, errNotLinux }
func (*FastPath) Pinned() ([]demux.Forward, error)                { return nil, errNotLinux }
func (*FastPath) EnsureOrder() error                              { return errNotLinux }
func (*FastPath) Ready() error                                    { return errNotLinux }
func (*FastPath) Stats() Stats                                    { return nil }
func (*FastPath) Close() error                                    { return nil }
