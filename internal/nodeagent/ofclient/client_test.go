package ofclient

import (
	"encoding/binary"
	"io"
	"net"
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

func TestOxmMetadata_Encoding(t *testing.T) {
	b := OxmMetadata(0x1234)
	// OXM_OF_METADATA: class=0x8000, field=2, has_mask=0, length=8
	// = (0x8000 << 16) | (2 << 9) | 8 = 0x80000000 | 0x400 | 0x08 = 0x80000408
	if got := binary.BigEndian.Uint32(b[0:4]); got != 0x80000408 {
		t.Errorf("OxmMetadata header = 0x%08x, want 0x80000408 (field=2, len=8)", got)
	}
	if got := binary.BigEndian.Uint64(b[4:12]); got != 0x1234 {
		t.Errorf("OxmMetadata value = %d, want 0x1234", got)
	}
	if len(b) != 12 {
		t.Errorf("OxmMetadata len = %d, want 12", len(b))
	}
}

func TestOxmReg0_Encoding(t *testing.T) {
	b := OxmReg0(5)
	// NXM_NX_REG0: class=0x0001, field=0, has_mask=0, length=4 = 0x00010004
	if got := binary.BigEndian.Uint32(b[0:4]); got != 0x00010004 {
		t.Errorf("OxmReg0 header = 0x%08x, want 0x00010004", got)
	}
}

// flowMod is the header fields of an encoded OFPT_FLOW_MOD.
type flowMod struct {
	table, cmd uint8
	priority   uint16
	outPort    uint32
	instr      int
}

func decodeFlowMod(t *testing.T, msg []byte) flowMod {
	t.Helper()
	if len(msg) < 48 || msg[1] != ofptFlowMod || int(binary.BigEndian.Uint16(msg[2:4])) != len(msg) {
		t.Fatalf("not a flow mod: % x", msg)
	}
	// 8 header, 16 cookie+mask, table, command, 2+2 timeouts, priority, buffer, out_port, out_group, flags+pad, match
	fm := flowMod{table: msg[24], cmd: msg[25], priority: binary.BigEndian.Uint16(msg[30:32]), outPort: binary.BigEndian.Uint32(msg[36:40])}
	matchLen := int(binary.BigEndian.Uint16(msg[48+2 : 48+4]))
	_ = matchLen
	fm.instr = len(msg) - 48 - ((matchLen+7)/8)*8
	return fm
}

func pipeClient(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return &Client{conn: a, portMap: map[string]uint32{}}, b
}

func read(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatal(err)
	}
	msg := make([]byte, binary.BigEndian.Uint16(hdr[2:4]))
	copy(msg, hdr)
	if _, err := io.ReadFull(conn, msg[8:]); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestFlowMods_DefaultDropIsAnAddWithoutActions(t *testing.T) {
	c, srv := pipeClient(t)
	go c.FlowAdd(0, 0, BuildMatchAdvanced(0, 0, false, 0, false, 0, false, 0, false), nil)
	fm := decodeFlowMod(t, read(t, srv))
	if fm.cmd != ofpfcAdd || fm.table != 0 || fm.priority != 0 || fm.instr != 0 || fm.outPort != ofppAny {
		t.Fatalf("a drop is an add of priority 0 with no instruction: %+v", fm)
	}
}

func TestFlowMods_DeleteVariants(t *testing.T) {
	c, srv := pipeClient(t)
	empty := BuildMatchAdvanced(0, 0, false, 0, false, 0, false, 0, false)
	for name, tc := range map[string]struct {
		send func()
		want flowMod
	}{
		"whole table":   {func() { c.FlowDeleteTable(6) }, flowMod{table: 6, cmd: ofpfcDelete, outPort: ofppAny}},
		"strict":        {func() { c.FlowDeleteStrict(0, 90, BuildMatch(5, 0, false)) }, flowMod{table: 0, cmd: ofpfcDeleteStrict, priority: 90, outPort: ofppAny}},
		"by output":     {func() { c.FlowDeleteOutPort(12) }, flowMod{table: ofpttAll, cmd: ofpfcDelete, outPort: 12}},
		"plain (match)": {func() { c.FlowDelete(0, empty) }, flowMod{table: 0, cmd: ofpfcDelete, outPort: ofppAny}},
	} {
		go tc.send()
		got := decodeFlowMod(t, read(t, srv))
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestStalePortNumbers(t *testing.T) {
	before := map[string]uint32{"a": 1, "b": 2, "c": 3, "geneve": 9}
	after := map[string]uint32{"a": 1, "c": 7, "geneve": 9, "d": 2}
	// b is gone, and c changed its number (3 is stale; 7 is new)
	got := StalePortNumbers(before, after)
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("stale = %v, want [2 3]", got)
	}
	if got := StalePortNumbers(before, before); len(got) != 0 {
		t.Fatalf("nothing changed: %v", got)
	}
}
