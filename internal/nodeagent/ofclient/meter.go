package ofclient

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// OpenFlow 1.3 meters. The kernel datapath of Open vSwitch supports one band per meter, and only the drop type, counted in kbit/s
// (measured on OVS 3.7.1 with the kernel datapath: max_meter 200000, max_bands 1, band_types drop, kbps pktps burst stats).
const (
	ofptMeterMod = 29

	ofpmcAdd    = 0
	ofpmcModify = 1
	ofpmcDelete = 2

	ofpmfKbps  = 0x1
	ofpmfBurst = 0x4
	ofpmfStats = 0x8

	ofpmbtDrop = 1

	ofpitGotoTable = 1
	ofpitMeter     = 6

	// OFPET_METER_MOD_FAILED and its codes (OpenFlow 1.3, ofp_meter_mod_failed_code).
	ofpetMeterModFailed    = 12
	ofpmmfcMeterExists     = 1
	ofpmmfcUnknownMeter    = 3
	checkedExchangeTimeout = 5 * time.Second
)

// OFError is an OFPT_ERROR the switch sent for a message of an exchange that waited for its barrier.
type OFError struct {
	Op   string
	Type uint16
	Code uint16
}

func (e *OFError) Error() string {
	op := e.Op
	if op == "" {
		op = "port retirement"
	}
	return fmt.Sprintf("%s rejected: OFPT_ERROR type=%d code=%d", op, e.Type, e.Code)
}

// unknownMeter says the switch does not have the meter a modify or delete named.
func unknownMeter(err error) bool {
	var e *OFError
	return errors.As(err, &e) && e.Type == ofpetMeterModFailed && e.Code == ofpmmfcUnknownMeter
}

// meterExists says an add named a meter that is already there.
func meterExists(err error) bool {
	var e *OFError
	return errors.As(err, &e) && e.Type == ofpetMeterModFailed && e.Code == ofpmmfcMeterExists
}

// buildMeterMod encodes an OFPT_METER_MOD with one drop band (delete has none).
func buildMeterMod(command uint16, id uint32, kbps, burstKbit uint32) []byte {
	n := 16
	if command != ofpmcDelete {
		n += 16
	}
	msg := make([]byte, n)
	putHeader(msg, ofptMeterMod, n)
	binary.BigEndian.PutUint16(msg[8:10], command)
	binary.BigEndian.PutUint16(msg[10:12], ofpmfKbps|ofpmfBurst|ofpmfStats)
	binary.BigEndian.PutUint32(msg[12:16], id)
	if command != ofpmcDelete {
		binary.BigEndian.PutUint16(msg[16:18], ofpmbtDrop)
		binary.BigEndian.PutUint16(msg[18:20], 16)
		binary.BigEndian.PutUint32(msg[20:24], kbps)
		binary.BigEndian.PutUint32(msg[24:28], burstKbit)
	}
	return msg
}

// Instructions are the instructions of a flow, encoded in the order the protocol requires: the meter, the actions, the next table.
type Instructions struct {
	// Meter is the meter id the packet goes through first (0 = none; ids start at 1).
	Meter uint32
	// Actions are applied after the meter.
	Actions []byte
	// GotoTable continues in that table (0 = no; table 0 is never a target).
	GotoTable uint8
}

func (i Instructions) encode() []byte {
	var out []byte
	if i.Meter != 0 {
		m := make([]byte, 8)
		binary.BigEndian.PutUint16(m[0:2], ofpitMeter)
		binary.BigEndian.PutUint16(m[2:4], 8)
		binary.BigEndian.PutUint32(m[4:8], i.Meter)
		out = append(out, m...)
	}
	if len(i.Actions) > 0 {
		out = append(out, buildInstruction(ofpitApplyActions, i.Actions)...)
	}
	if i.GotoTable != 0 {
		g := make([]byte, 8)
		binary.BigEndian.PutUint16(g[0:2], ofpitGotoTable)
		binary.BigEndian.PutUint16(g[2:4], 8)
		g[4] = i.GotoTable
		out = append(out, g...)
	}
	return out
}

// exchange sends the messages and a barrier and returns the first error the switch answers for them, or nil when the barrier
// comes back without one. It uses the machinery of the retirements, so it never overlaps with one.
func (c *Client) exchange(op string, msgs ...[]byte) error {
	c.retireMu.Lock()
	defer c.retireMu.Unlock()
	ch := make(chan error, 1)
	barrier := buildHeader(20)
	c.writeMu.Lock()
	xids := map[uint32]bool{}
	for _, m := range msgs {
		c.stamp(m)
		xids[binary.BigEndian.Uint32(m[4:8])] = true
	}
	c.stamp(barrier)
	c.pdMu.Lock()
	c.pendingRetire = ch
	c.retireXIDs = xids
	c.retireBarrier = binary.BigEndian.Uint32(barrier[4:8])
	c.retireError = nil
	c.retireOp = op
	c.pdMu.Unlock()
	var writeErr error
	for _, m := range append(append([][]byte(nil), msgs...), barrier) {
		if _, writeErr = c.conn.Write(m); writeErr != nil {
			break
		}
	}
	c.writeMu.Unlock()
	defer func() {
		c.pdMu.Lock()
		c.pendingRetire, c.retireXIDs, c.retireOp = nil, nil, ""
		c.pdMu.Unlock()
	}()
	if writeErr != nil {
		return writeErr
	}
	timer := time.NewTimer(checkedExchangeTimeout)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-timer.C:
		return fmt.Errorf("%s: no barrier reply in %s", op, checkedExchangeTimeout)
	}
}

// MeterSet makes the meter exist with this rate (kbit/s) and burst (kbit): a modify, and an add when the switch does not have it
// yet. It never deletes: deleting a meter deletes the flows that use it. The meter is confirmed by a barrier.
func (c *Client) MeterSet(id, kbps, burstKbit uint32) error {
	err := c.exchange("meter modify", buildMeterMod(ofpmcModify, id, kbps, burstKbit))
	if !unknownMeter(err) {
		return err
	}
	err = c.exchange("meter add", buildMeterMod(ofpmcAdd, id, kbps, burstKbit))
	if meterExists(err) { // someone added it between the two messages: that one is as good
		return nil
	}
	return err
}

// MeterDelete removes the meter and, with it, every flow that uses it; a meter that is not there is not an error.
func (c *Client) MeterDelete(id uint32) error {
	err := c.exchange("meter delete", buildMeterMod(ofpmcDelete, id, 0, 0))
	if unknownMeter(err) {
		return nil
	}
	return err
}

// FlowAddInstr adds a flow with the given instructions and confirms it with a barrier, so a flow that names a meter the switch
// does not have is reported here and not lost.
func (c *Client) FlowAddInstr(tableID uint8, priority uint16, match []byte, ins Instructions) error {
	return c.exchange("flow add", buildFlowModInstr(ofpfcAdd, tableID, priority, match, ins.encode(), ofppAny))
}
