// Package ofclient implements a minimal OpenFlow 1.3 client over a Unix socket.
// Supports: Hello handshake, PORT_DESC multipart for name→number resolution,
// FLOW_MOD add and delete with OXM_OF_IN_PORT and OXM_OF_TUNNEL_ID fields.
package ofclient

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// OpenFlow 1.3 message types.
const (
	ofptHello            = 0
	ofptFeaturesRequest  = 5
	ofptFeaturesReply    = 6
	ofptFlowMod          = 14
	ofptMultipartRequest = 18
	ofptMultipartReply   = 19
)

// OFPMP multipart types.
const ofpmpPortDesc = 13

// OFPFC flow mod commands.
const (
	ofpfcAdd    = 0
	ofpfcDelete = 3
)

// OFPIT instruction types.
const ofpitApplyActions = 4

// OFPAT action types.
const (
	ofpatOutput   = 0
	ofpatSetField = 25
	ofpatGroup    = 22
)

// OFPG group identifiers.
const ofpgNormal = 0xfffffffc

// OFPP special port numbers.
const (
	ofppAny     = 0xffffffff
	ofpNoBuffer = 0xffffffff
)

// Client is a minimal OpenFlow 1.3 client connected to an OVS bridge management socket.
type Client struct {
	conn    net.Conn
	mu      sync.Mutex
	xid     atomic.Uint32
	portMap map[string]uint32 // port name → port number
}

// Connect connects to the OVS bridge management socket and performs the OF 1.3 handshake.
// sockPath is e.g. "/run/openvswitch/br-ovs.mgmt".
// Retries for up to 30 seconds (socket appears after vswitchd creates the bridge).
func Connect(sockPath string) (*Client, error) {
	var conn net.Conn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to OF socket %s: %w", sockPath, err)
	}

	c := &Client{conn: conn, portMap: make(map[string]uint32)}
	if err := c.handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// PortNo returns the OpenFlow port number for a named port, or error if unknown.
func (c *Client) PortNo(name string) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	no, ok := c.portMap[name]
	if !ok {
		return 0, fmt.Errorf("ofclient: port %q not found; known ports: %v", name, c.portMap)
	}
	return no, nil
}

// RefreshPorts re-sends PORT_DESC multipart and updates the portMap.
func (c *Client) RefreshPorts() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queryPortDesc()
}

// FlowAdd sends an OFPT_FLOW_MOD OFPFC_ADD for a flow with the given match and actions.
func (c *Client) FlowAdd(tableID uint8, priority uint16, match, actions []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendFlowMod(ofpfcAdd, tableID, priority, match, actions)
}

// FlowDelete sends an OFPT_FLOW_MOD OFPFC_DELETE (non-strict) for a flow matching match.
func (c *Client) FlowDelete(tableID uint8, match []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendFlowMod(ofpfcDelete, tableID, 0, match, nil)
}

func (c *Client) handshake() error {
	// Send OFPT_HELLO.
	if err := c.send(buildHeader(ofptHello, 8)); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	// Read server OFPT_HELLO (ignore body).
	if _, err := c.recv(); err != nil {
		return fmt.Errorf("recv hello: %w", err)
	}
	// Query port descriptions.
	return c.queryPortDesc()
}

func (c *Client) queryPortDesc() error {
	// OFPT_MULTIPART_REQUEST for OFPMP_PORT_DESC.
	req := make([]byte, 16) // 8 header + 4 type+flags + 4 pad
	putHeader(req, ofptMultipartRequest, 16)
	binary.BigEndian.PutUint16(req[8:10], ofpmpPortDesc)
	// flags and pad are zero

	if err := c.send(req); err != nil {
		return fmt.Errorf("send PORT_DESC request: %w", err)
	}

	// Read reply (may be multi-part; flags bit 0 = more).
	newPortMap := make(map[string]uint32)
	for {
		msg, err := c.recv()
		if err != nil {
			return fmt.Errorf("recv PORT_DESC reply: %w", err)
		}
		if msg[1] != ofptMultipartReply {
			continue
		}
		if binary.BigEndian.Uint16(msg[8:10]) != ofpmpPortDesc {
			continue
		}
		// Parse port entries. Each ofp_port is 64 bytes.
		// Reply body starts at byte 16 (after 8 header + 4 type+flags + 4 pad).
		body := msg[16:]
		for len(body) >= 64 {
			portNo := binary.BigEndian.Uint32(body[0:4])
			// name is at offset 16, 16 bytes, NUL-terminated.
			nameBytes := body[16:32]
			n := 0
			for n < 16 && nameBytes[n] != 0 {
				n++
			}
			name := string(nameBytes[:n])
			if name != "" {
				newPortMap[name] = portNo
			}
			body = body[64:]
		}
		flags := binary.BigEndian.Uint16(msg[10:12])
		if flags&0x01 == 0 { // OFPMPF_REPLY_MORE = 0x01
			break
		}
	}
	c.portMap = newPortMap
	return nil
}

func (c *Client) sendFlowMod(cmd uint8, tableID uint8, priority uint16, match, actions []byte) error {
	var instr []byte
	if len(actions) > 0 {
		instr = buildInstruction(ofpitApplyActions, actions)
	}
	bodyLen := 40 + len(match) + len(instr)
	totalLen := 8 + bodyLen
	msg := make([]byte, totalLen)
	putHeader(msg, ofptFlowMod, totalLen)

	off := 8
	// cookie, cookie_mask (8+8 = 16 bytes, all zero)
	off += 16
	msg[off] = tableID // table_id
	off++
	msg[off] = cmd // command
	off++
	// idle_timeout, hard_timeout (2+2, zero)
	off += 4
	binary.BigEndian.PutUint16(msg[off:], priority)
	off += 2
	binary.BigEndian.PutUint32(msg[off:], ofpNoBuffer) // buffer_id
	off += 4
	binary.BigEndian.PutUint32(msg[off:], ofppAny) // out_port
	off += 4
	binary.BigEndian.PutUint32(msg[off:], ofppAny) // out_group
	off += 4
	// flags, pad (2+2, zero)
	off += 4
	copy(msg[off:], match)
	off += len(match)
	copy(msg[off:], instr)

	return c.send(msg)
}

// send writes msg to the connection. Callers must hold c.mu or call before concurrent access begins.
func (c *Client) send(msg []byte) error {
	xid := c.xid.Add(1)
	binary.BigEndian.PutUint32(msg[4:8], xid)
	_, err := c.conn.Write(msg)
	return err
}

func (c *Client) recv() ([]byte, error) {
	hdr := make([]byte, 8)
	if _, err := readFull(c.conn, hdr); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(hdr[2:4]))
	if length < 8 {
		return nil, fmt.Errorf("ofclient: invalid message length %d", length)
	}
	msg := make([]byte, length)
	copy(msg, hdr)
	if length > 8 {
		if _, err := readFull(c.conn, msg[8:]); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

// --- Wire encoding helpers (exported for use by openflow.go) ---

// buildHeader returns an OF 1.3 header slice of totalLen bytes with version=4.
func buildHeader(msgType uint8, totalLen int) []byte {
	b := make([]byte, totalLen)
	putHeader(b, msgType, totalLen)
	return b
}

func putHeader(b []byte, msgType uint8, totalLen int) {
	b[0] = 4 // version = OF 1.3
	b[1] = msgType
	binary.BigEndian.PutUint16(b[2:4], uint16(totalLen))
	// xid filled in by send()
}

// BuildMatch builds an ofp_match (OXM type) for in_port and optionally tunnel_id.
// Result is padded to a multiple of 8 bytes.
func BuildMatch(inPortNo uint32, vni uint64, hasTunnel bool) []byte {
	var oxmFields []byte
	oxmFields = append(oxmFields, OxmInPort(inPortNo)...)
	if hasTunnel {
		oxmFields = append(oxmFields, OxmTunnelID(vni)...)
	}
	// ofp_match: type(2) + length(2) + oxm_fields + padding to 8-byte boundary
	rawLen := 4 + len(oxmFields)
	padded := (rawLen + 7) &^ 7
	m := make([]byte, padded)
	binary.BigEndian.PutUint16(m[0:2], 1)              // OFPMT_OXM
	binary.BigEndian.PutUint16(m[2:4], uint16(rawLen)) // length without padding
	copy(m[4:], oxmFields)
	return m
}

// OxmInPort encodes OXM_OF_IN_PORT (class=0x8000, field=0, length=4).
// Returns 8 bytes: 4-byte header + 4-byte value.
func OxmInPort(portNo uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], 0x80000004) // class=0x8000, field=0, hasmask=0, len=4
	binary.BigEndian.PutUint32(b[4:8], portNo)
	return b
}

// OxmTunnelID encodes OXM_OF_TUNNEL_ID (class=0x8000, field=38, length=8).
// Returns 12 bytes: 4-byte header + 8-byte value.
func OxmTunnelID(vni uint64) []byte {
	b := make([]byte, 12)
	// (0x8000 << 16) | (38 << 9) | (0 << 8) | 8 = 0x80004C08
	binary.BigEndian.PutUint32(b[0:4], 0x80004C08)
	binary.BigEndian.PutUint64(b[4:12], vni)
	return b
}

// BuildActionsOutput encodes a single OFPAT_OUTPUT action (16 bytes).
func BuildActionsOutput(portNo uint32) []byte {
	a := make([]byte, 16)
	binary.BigEndian.PutUint16(a[0:2], ofpatOutput)
	binary.BigEndian.PutUint16(a[2:4], 16)
	binary.BigEndian.PutUint32(a[4:8], portNo)
	binary.BigEndian.PutUint16(a[8:10], 0xffff) // max_len = OFPCML_NO_BUFFER
	return a
}

// BuildActionsSetFieldTunnelID encodes OFPAT_SET_FIELD for tunnel_id=vni (16 bytes).
func BuildActionsSetFieldTunnelID(vni uint64) []byte {
	// 4 (action header) + 12 (OXM TUNNEL_ID: 4 hdr + 8 value) = 16 bytes, already 8-aligned.
	a := make([]byte, 16)
	binary.BigEndian.PutUint16(a[0:2], ofpatSetField)
	binary.BigEndian.PutUint16(a[2:4], 16)
	copy(a[4:], OxmTunnelID(vni))
	return a
}

// BuildActionsGroupNormal encodes OFPAT_GROUP with group=OFPG_NORMAL (8 bytes).
func BuildActionsGroupNormal() []byte {
	a := make([]byte, 8)
	binary.BigEndian.PutUint16(a[0:2], ofpatGroup)
	binary.BigEndian.PutUint16(a[2:4], 8)
	binary.BigEndian.PutUint32(a[4:8], ofpgNormal)
	return a
}

// buildInstruction wraps actions in an OFPIT instruction.
func buildInstruction(instrType uint16, actions []byte) []byte {
	totalLen := 8 + len(actions)
	instr := make([]byte, totalLen)
	binary.BigEndian.PutUint16(instr[0:2], instrType)
	binary.BigEndian.PutUint16(instr[2:4], uint16(totalLen))
	copy(instr[8:], actions)
	return instr
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	return io.ReadFull(conn, buf)
}
