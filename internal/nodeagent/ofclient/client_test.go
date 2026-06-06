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

func TestBuildActionsSetFieldTunnelID_Encoding(t *testing.T) {
	a := BuildActionsSetFieldTunnelID(999)
	// OFPAT_SET_FIELD: type=25(2) + len=16(2) + OXM_TUNNEL_ID(12) = 16 bytes total
	if len(a) != 16 {
		t.Errorf("SetField action len = %d, want 16", len(a))
	}
	if binary.BigEndian.Uint16(a[0:2]) != 25 {
		t.Errorf("action type = %d, want 25 (OFPAT_SET_FIELD)", binary.BigEndian.Uint16(a[0:2]))
	}
	if binary.BigEndian.Uint16(a[2:4]) != 16 {
		t.Errorf("action len = %d, want 16", binary.BigEndian.Uint16(a[2:4]))
	}
	// OXM header for TUNNEL_ID embedded at a[4:8]
	if binary.BigEndian.Uint32(a[4:8]) != 0x80004C08 {
		t.Errorf("OXM header in SET_FIELD = 0x%08x, want 0x80004C08", binary.BigEndian.Uint32(a[4:8]))
	}
	// Value at a[8:16]
	if binary.BigEndian.Uint64(a[8:16]) != 999 {
		t.Errorf("tunnel_id value = %d, want 999", binary.BigEndian.Uint64(a[8:16]))
	}
}

func TestBuildActionsGroupNormal_Encoding(t *testing.T) {
	a := BuildActionsGroupNormal()
	// OFPAT_GROUP: type=22(2) + len=8(2) + group_id=OFPG_NORMAL(4) = 8 bytes
	if len(a) != 8 {
		t.Errorf("group action len = %d, want 8", len(a))
	}
	if binary.BigEndian.Uint16(a[0:2]) != 22 {
		t.Errorf("action type = %d, want 22 (OFPAT_GROUP)", binary.BigEndian.Uint16(a[0:2]))
	}
	if binary.BigEndian.Uint32(a[4:8]) != 0xfffffffc {
		t.Errorf("group_id = 0x%08x, want 0xfffffffc (OFPG_NORMAL)", binary.BigEndian.Uint32(a[4:8]))
	}
}

func TestBuildActionsOutput_MultipleActions(t *testing.T) {
	// Verify actions can be concatenated correctly (egress flow: SET_FIELD + OUTPUT)
	setField := BuildActionsSetFieldTunnelID(42)
	output := BuildActionsOutput(5)
	actions := append(setField, output...)
	if len(actions) != 32 { // 16 + 16
		t.Errorf("combined actions len = %d, want 32", len(actions))
	}
	// First action is SET_FIELD
	if binary.BigEndian.Uint16(actions[0:2]) != 25 {
		t.Errorf("first action type = %d, want 25", binary.BigEndian.Uint16(actions[0:2]))
	}
	// Second action is OUTPUT
	if binary.BigEndian.Uint16(actions[16:18]) != 0 {
		t.Errorf("second action type = %d, want 0 (OUTPUT)", binary.BigEndian.Uint16(actions[16:18]))
	}
}
