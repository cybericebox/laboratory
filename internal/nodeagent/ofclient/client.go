// Package ofclient implements a minimal OpenFlow 1.3 client over a Unix socket.
// Supports: Hello handshake, PORT_DESC multipart for name→number resolution,
// FLOW_MOD add and delete with OXM_OF_IN_PORT and OXM_OF_TUNNEL_ID fields,
// and automatic OFPT_ECHO_REPLY to keep the OVS connection alive.
package ofclient

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// OpenFlow 1.3 message types.
const (
	ofptHello            = 0
	ofptError            = 1
	ofptEchoRequest      = 2
	ofptEchoReply        = 3
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
	ofpfcAdd          = 0
	ofpfcDelete       = 3
	ofpfcDeleteStrict = 4
)

// ofpttAll addresses every table in a flow delete.
const ofpttAll = 0xff

// OFPIT instruction types.
const ofpitApplyActions = 4

// OFPAT action types.
const (
	ofpatOutput   = 0
	ofpatSetField = 25
)

// OFPP special port numbers.
const (
	ofppAny     = 0xffffffff
	ofpNoBuffer = 0xffffffff
)

// portDescMsg carries one chunk of a PORT_DESC multipart reply from the background reader.
type portDescMsg struct {
	body []byte
	more bool
	err  error
}

// Client is a minimal OpenFlow 1.3 client connected to an OVS bridge management socket.
// A background goroutine reads all incoming messages, replies to OFPT_ECHO_REQUEST
// automatically, and dispatches PORT_DESC replies to a waiting queryPortDesc call.
type Client struct {
	conn    net.Conn
	writeMu sync.Mutex // serialises all writes; never held while blocked on recv

	xid atomic.Uint32

	mapMu   sync.RWMutex
	portMap map[string]uint32 // port name → port number

	queryMu sync.Mutex // serialises queryPortDesc: only one PORT_DESC in flight

	pdMu       sync.Mutex
	pendingPD  chan portDescMsg // non-nil while queryPortDesc is active
	pendingXID uint32           // xid of the in-flight PORT_DESC request

	// onError, if set, is invoked for every OFPT_ERROR not consumed by a
	// pending PORT_DESC query (e.g. a rejected FLOW_MOD). Set via SetErrorHandler.
	onError func(xid uint32, errType, errCode uint16)

	closeOnce sync.Once
}

// SetErrorHandler registers a callback for asynchronous OFPT_ERROR messages
// (rejected FLOW_MODs, etc.). Must be set before flows are programmed.
func (c *Client) SetErrorHandler(h func(xid uint32, errType, errCode uint16)) {
	c.onError = h
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

	// HELLO exchange runs synchronously before background reader starts.
	if err := c.handshake(); err != nil {
		conn.Close()
		return nil, err
	}

	// Background reader: handles echo replies, dispatches PORT_DESC chunks.
	go c.readLoop()

	// Initial PORT_DESC query via channel (readLoop is now running).
	if err := c.queryPortDesc(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the connection and stops the background reader.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.conn.Close() })
	return err
}

// PortNo returns the OpenFlow port number for a named port, or error if unknown.
func (c *Client) PortNo(name string) (uint32, error) {
	c.mapMu.RLock()
	defer c.mapMu.RUnlock()
	no, ok := c.portMap[name]
	if !ok {
		return 0, fmt.Errorf("ofclient: port %q not found; known ports: %v", name, c.portMap)
	}
	return no, nil
}

// RefreshPorts re-sends PORT_DESC multipart and updates the portMap.
func (c *Client) RefreshPorts() error {
	return c.queryPortDesc()
}

// FlowAdd sends an OFPT_FLOW_MOD OFPFC_ADD for a flow with the given match and actions.
func (c *Client) FlowAdd(tableID uint8, priority uint16, match, actions []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.sendFlowMod(ofpfcAdd, tableID, priority, match, actions)
}

// FlowDelete sends an OFPT_FLOW_MOD OFPFC_DELETE (non-strict) for a flow matching match.
func (c *Client) FlowDelete(tableID uint8, match []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.sendFlowMod(ofpfcDelete, tableID, 0, match, nil)
}

// FlowDeleteStrict deletes the one flow with exactly this match and priority.
func (c *Client) FlowDeleteStrict(tableID uint8, priority uint16, match []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.sendFlowMod(ofpfcDeleteStrict, tableID, priority, match, nil)
}

// FlowDeleteTable deletes every flow of a table.
func (c *Client) FlowDeleteTable(tableID uint8) error {
	return c.FlowDelete(tableID, BuildMatchAdvanced(0, 0, false, 0, false, 0, false, 0, false))
}

// FlowDeleteOutPort deletes the flows of every table that output to the port number.
func (c *Client) FlowDeleteOutPort(portNo uint32) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	msg := buildFlowMod(ofpfcDelete, ofpttAll, 0, BuildMatchAdvanced(0, 0, false, 0, false, 0, false, 0, false), nil, portNo)
	c.stamp(msg)
	_, err := c.conn.Write(msg)
	return err
}

// Ports returns a copy of the port map (name to number) as of the last refresh.
func (c *Client) Ports() map[string]uint32 {
	c.mapMu.RLock()
	defer c.mapMu.RUnlock()
	out := make(map[string]uint32, len(c.portMap))
	for k, v := range c.portMap {
		out[k] = v
	}
	return out
}

// StalePortNumbers lists the port numbers that were in use before a refresh and are not the same port after it:
// the port is gone, or the number now belongs to another name. Flows that name such a number must be removed
// before the number is used again.
func StalePortNumbers(before, after map[string]uint32) []uint32 {
	var stale []uint32
	for name, no := range before {
		if now, ok := after[name]; !ok || now != no {
			stale = append(stale, no)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i] < stale[j] })
	return stale
}

// readLoop runs as a goroutine. It reads all incoming OF messages and:
//   - replies to OFPT_ECHO_REQUEST (keeps OVS connection alive)
//   - dispatches OFPT_MULTIPART_REPLY PORT_DESC chunks to a waiting queryPortDesc call
//
// Exits when the connection is closed.
func (c *Client) readLoop() {
	for {
		msg, err := c.rawRecv()
		if err != nil {
			// Connection closed (or fatal read error) — notify any waiting query.
			c.pdMu.Lock()
			if c.pendingPD != nil {
				select {
				case c.pendingPD <- portDescMsg{err: err}:
				default:
				}
			}
			c.pdMu.Unlock()
			return
		}

		switch msg[1] {
		case ofptEchoRequest:
			// Reply with same body, changing only the type byte.
			reply := make([]byte, len(msg))
			copy(reply, msg)
			reply[1] = ofptEchoReply
			c.writeMu.Lock()
			_, _ = c.conn.Write(reply) // best-effort; ignore error
			c.writeMu.Unlock()

		case ofptMultipartReply:
			if len(msg) < 12 {
				continue
			}
			if binary.BigEndian.Uint16(msg[8:10]) != ofpmpPortDesc {
				continue
			}
			var body []byte
			if len(msg) > 16 {
				body = make([]byte, len(msg)-16)
				copy(body, msg[16:])
			}
			flags := binary.BigEndian.Uint16(msg[10:12])
			// Non-blocking send: a blocking send while holding pdMu deadlocks
			// against queryPortDesc's deferred pendingPD=nil (which needs pdMu)
			// if the query already returned. The buffer is sized so a drop only
			// happens for a stale/abandoned query — that query times out and the
			// caller retries with a fresh one.
			c.pdMu.Lock()
			if c.pendingPD != nil {
				select {
				case c.pendingPD <- portDescMsg{body: body, more: flags&0x01 != 0}:
				default:
				}
			}
			c.pdMu.Unlock()

		case ofptError:
			// Only propagate to a pending PORT_DESC query when the error's xid
			// matches the in-flight request. Errors for other requests (e.g. a
			// FlowMod rejected by OVS) must not fail an unrelated PORT_DESC —
			// but they must not be silent either: FLOW_MOD is fire-and-forget,
			// so a rejected flow would otherwise vanish without a trace.
			if len(msg) >= 12 {
				errXID := binary.BigEndian.Uint32(msg[4:8])
				errType := binary.BigEndian.Uint16(msg[8:10])
				errCode := binary.BigEndian.Uint16(msg[10:12])
				c.pdMu.Lock()
				matchedPD := c.pendingPD != nil && errXID == c.pendingXID
				if matchedPD {
					select {
					case c.pendingPD <- portDescMsg{err: fmt.Errorf("OFPT_ERROR type=%d code=%d", errType, errCode)}:
					default:
					}
				}
				c.pdMu.Unlock()
				if !matchedPD && c.onError != nil {
					c.onError(errXID, errType, errCode)
				}
			}
		}
	}
}

// handshake performs the OF 1.3 HELLO exchange synchronously, before readLoop starts.
func (c *Client) handshake() error {
	hello := buildHeader(ofptHello, 8)
	xid := c.xid.Add(1)
	binary.BigEndian.PutUint32(hello[4:8], xid)
	if _, err := c.conn.Write(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	// Read until we get a HELLO back (ignore other messages at this stage).
	for {
		msg, err := c.rawRecv()
		if err != nil {
			return fmt.Errorf("recv hello: %w", err)
		}
		if msg[1] == ofptHello {
			return nil
		}
	}
}

// queryPortDesc sends a PORT_DESC multipart request and collects the replies via the
// channel that readLoop dispatches to. Safe to call concurrently with readLoop.
func (c *Client) queryPortDesc() error {
	// Serialise: OVS rejects overlapping multipart requests on one connection,
	// and a second in-flight PORT_DESC would clobber pendingPD.
	c.queryMu.Lock()
	defer c.queryMu.Unlock()

	ch := make(chan portDescMsg, 64)

	req := make([]byte, 16) // 8 header + 4 type+flags + 4 pad
	putHeader(req, ofptMultipartRequest, 16)
	binary.BigEndian.PutUint16(req[8:10], ofpmpPortDesc)

	c.writeMu.Lock()
	xid := c.xid.Add(1)
	binary.BigEndian.PutUint32(req[4:8], xid)
	// Register the pending query under the write lock so the reply cannot be
	// dispatched (and dropped) before pendingPD is set.
	c.pdMu.Lock()
	c.pendingPD = ch
	c.pendingXID = xid
	c.pdMu.Unlock()
	_, writeErr := c.conn.Write(req)
	c.writeMu.Unlock()
	defer func() {
		c.pdMu.Lock()
		c.pendingPD = nil
		c.pdMu.Unlock()
	}()
	if writeErr != nil {
		return fmt.Errorf("send PORT_DESC request: %w", writeErr)
	}

	newPortMap := make(map[string]uint32)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		var m portDescMsg
		var ok bool
		select {
		case m, ok = <-ch:
		case <-timeout.C:
			// No reply (dropped chunk, wedged OVS, lost socket): fail the query
			// instead of blocking the caller — and everything behind queryMu — forever.
			return fmt.Errorf("ofclient: PORT_DESC reply timed out after 5s")
		}
		if !ok {
			return fmt.Errorf("ofclient: PORT_DESC channel closed unexpectedly")
		}
		if m.err != nil {
			return fmt.Errorf("recv PORT_DESC reply: %w", m.err)
		}
		body := m.body
		for len(body) >= 64 {
			portNo := binary.BigEndian.Uint32(body[0:4])
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
		if !m.more {
			break
		}
	}

	c.mapMu.Lock()
	c.portMap = newPortMap
	c.mapMu.Unlock()
	return nil
}

func (c *Client) sendFlowMod(cmd uint8, tableID uint8, priority uint16, match, actions []byte) error {
	msg := buildFlowMod(cmd, tableID, priority, match, actions, ofppAny)
	c.stamp(msg)
	_, err := c.conn.Write(msg)
	return err
}

// stamp gives the message a fresh xid.
func (c *Client) stamp(msg []byte) {
	binary.BigEndian.PutUint32(msg[4:8], c.xid.Add(1))
}

// buildFlowMod encodes an OFPT_FLOW_MOD. outPort filters deletes (ofppAny: no filter).
func buildFlowMod(cmd uint8, tableID uint8, priority uint16, match, actions []byte, outPort uint32) []byte {
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
	binary.BigEndian.PutUint32(msg[off:], outPort) // out_port
	off += 4
	binary.BigEndian.PutUint32(msg[off:], ofppAny) // out_group
	off += 4
	// flags, pad (2+2, zero)
	off += 4
	copy(msg[off:], match)
	off += len(match)
	copy(msg[off:], instr)
	return msg
}

// rawRecv reads one complete OF message from the connection.
// Only called from handshake (before readLoop) and from readLoop itself.
func (c *Client) rawRecv() ([]byte, error) {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(hdr[2:4]))
	if length < 8 {
		return nil, fmt.Errorf("ofclient: invalid message length %d", length)
	}
	msg := make([]byte, length)
	copy(msg, hdr)
	if length > 8 {
		if _, err := io.ReadFull(c.conn, msg[8:]); err != nil {
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
	// xid filled in by caller
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

// buildInstruction wraps actions in an OFPIT instruction.
func buildInstruction(instrType uint16, actions []byte) []byte {
	totalLen := 8 + len(actions)
	instr := make([]byte, totalLen)
	binary.BigEndian.PutUint16(instr[0:2], instrType)
	binary.BigEndian.PutUint16(instr[2:4], uint16(totalLen))
	copy(instr[8:], actions)
	return instr
}
