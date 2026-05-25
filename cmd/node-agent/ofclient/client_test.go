package ofclient

import (
	"encoding/binary"
	"testing"
)

func TestOxmInPort_Encoding(t *testing.T) {
	b := OxmInPort(7)
	// OXM header: class=0x8000, field=0, has_mask=0, length=4 → 0x80000004
	if got := binary.BigEndian.Uint32(b[0:4]); got != 0x80000004 {
		t.Errorf("OxmInPort header = 0x%08x, want 0x80000004", got)
	}
	if got := binary.BigEndian.Uint32(b[4:8]); got != 7 {
		t.Errorf("OxmInPort value = %d, want 7", got)
	}
	if len(b) != 8 {
		t.Errorf("OxmInPort len = %d, want 8", len(b))
	}
}

func TestOxmTunnelID_Encoding(t *testing.T) {
	b := OxmTunnelID(100)
	// OXM header: class=0x8000, field=38, has_mask=0, length=8
	// = (0x8000 << 16) | (38 << 9) | 8 = 0x80000000 | 0x4C00 | 0x08 = 0x80004C08
	if got := binary.BigEndian.Uint32(b[0:4]); got != 0x80004C08 {
		t.Errorf("OxmTunnelID header = 0x%08x, want 0x80004C08", got)
	}
	if got := binary.BigEndian.Uint64(b[4:12]); got != 100 {
		t.Errorf("OxmTunnelID value = %d, want 100", got)
	}
	if len(b) != 12 {
		t.Errorf("OxmTunnelID len = %d, want 12", len(b))
	}
}

func TestBuildMatch_InPortOnly(t *testing.T) {
	m := BuildMatch(3, 0, false)
	if len(m)%8 != 0 {
		t.Errorf("BuildMatch length %d not 8-byte aligned", len(m))
	}
	if binary.BigEndian.Uint16(m[0:2]) != 1 {
		t.Errorf("match type = %d, want 1 (OFPMT_OXM)", binary.BigEndian.Uint16(m[0:2]))
	}
}

func TestBuildMatch_InPortAndTunnel(t *testing.T) {
	m := BuildMatch(3, 42, true)
	if len(m)%8 != 0 {
		t.Errorf("BuildMatch length %d not 8-byte aligned", len(m))
	}
}

func TestBuildActionsOutput_Encoding(t *testing.T) {
	a := BuildActionsOutput(1)
	// OFPAT_OUTPUT: type=0(2) + len=16(2) + port(4) + max_len=0xffff(2) + pad(6) = 16 bytes
	if len(a) != 16 {
		t.Errorf("output action len = %d, want 16", len(a))
	}
	if binary.BigEndian.Uint16(a[0:2]) != 0 {
		t.Errorf("action type = %d, want 0 (OFPAT_OUTPUT)", binary.BigEndian.Uint16(a[0:2]))
	}
	if binary.BigEndian.Uint32(a[4:8]) != 1 {
		t.Errorf("port = %d, want 1", binary.BigEndian.Uint32(a[4:8]))
	}
}
