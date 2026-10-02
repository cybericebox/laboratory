// Nicira (NX) extensions used by the 7-table OpenFlow pipeline (spec §7).
//
// OXM matchers extend the standard set with metadata, registers, and tunnel
// metadata fields. Actions extend OFPAT with experimenter-encoded operations
// (resubmit, register move, register load) that live under
// vendor=NX_VENDOR_ID=0x00002320.
//
// Reference: openvswitch.h (OVS source tree), section "Nicira Extensions".

package ofclient

import "encoding/binary"

// NxVendorID is the experimenter ID assigned to the Nicira-extension family.
const NxVendorID = 0x00002320

// Nicira action subtypes used here. Values are stable across OVS versions.
const (
	nxastResubmitTable = 14
	nxastRegMove       = 6
)

// OXM type encoding: (class << 16) | (field << 9) | (hasmask << 8) | length.
// helper for readability.
func oxmHeader(class uint16, field uint8, hasMask bool, length uint8) uint32 {
	var hm uint32
	if hasMask {
		hm = 1
	}
	return uint32(class)<<16 | uint32(field)<<9 | hm<<8 | uint32(length)
}

// --- OXM matchers ---

// OxmMetadata encodes OXM_OF_METADATA (class=0x8000, field=2, 8 bytes).
// Used as the canonical VNI carrier across the pipeline. Field code 2 is
// OFPXMT_OFB_METADATA; field 4 is ETH_SRC and made OVS reject the FLOW_MOD.
func OxmMetadata(value uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint32(b[0:4], oxmHeader(0x8000, 2, false, 8))
	binary.BigEndian.PutUint64(b[4:12], value)
	return b
}

// OxmReg0 encodes NXM_NX_REG0 (class=0x0001, field=0, 4 bytes).
func OxmReg0(value uint32) []byte { return oxmReg(0, value) }

// OxmReg1 encodes NXM_NX_REG1 (class=0x0001, field=1, 4 bytes).
func OxmReg1(value uint32) []byte { return oxmReg(1, value) }

func oxmReg(reg uint8, value uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], oxmHeader(0x0001, reg, false, 4))
	binary.BigEndian.PutUint32(b[4:8], value)
	return b
}

// OxmTunIPv4Src encodes NXM_NX_TUN_IPV4_SRC (class=0x0001, field=31, 4 bytes).
func OxmTunIPv4Src(ip uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], oxmHeader(0x0001, 31, false, 4))
	binary.BigEndian.PutUint32(b[4:8], ip)
	return b
}

// OxmTunIPv4Dst encodes NXM_NX_TUN_IPV4_DST (class=0x0001, field=32, 4 bytes).
func OxmTunIPv4Dst(ip uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], oxmHeader(0x0001, 32, false, 4))
	binary.BigEndian.PutUint32(b[4:8], ip)
	return b
}

// --- OXM IDs (used by NXAST_REG_MOVE source/destination identifiers) ---

// OxmIDReg0 is the 4-byte header for NXM_NX_REG0 (no value).
func OxmIDReg0() uint32 { return oxmHeader(0x0001, 0, false, 4) }

// OxmIDReg1 is the 4-byte header for NXM_NX_REG1.
func OxmIDReg1() uint32 { return oxmHeader(0x0001, 1, false, 4) }

// OxmIDMetadata is the 4-byte header for OXM_OF_METADATA (field 2).
func OxmIDMetadata() uint32 { return oxmHeader(0x8000, 2, false, 8) }

// OxmIDTunnelID is the 4-byte header for OXM_OF_TUNNEL_ID.
func OxmIDTunnelID() uint32 { return oxmHeader(0x8000, 38, false, 8) }

// OxmIDTunIPv4Src is the 4-byte header for NXM_NX_TUN_IPV4_SRC.
func OxmIDTunIPv4Src() uint32 { return oxmHeader(0x0001, 31, false, 4) }

// OxmIDTunIPv4Dst is the 4-byte header for NXM_NX_TUN_IPV4_DST.
func OxmIDTunIPv4Dst() uint32 { return oxmHeader(0x0001, 32, false, 4) }

// OxmIDInPort is the 4-byte header for OXM_OF_IN_PORT.
func OxmIDInPort() uint32 { return oxmHeader(0x8000, 0, false, 4) }

// --- Set-field convenience wrappers ---

// BuildActionsSetReg0 encodes OFPAT_SET_FIELD for NXM_NX_REG0.
// 4-byte action header + 4 OXM hdr + 4 value = 12 bytes; pad to 16 (8-aligned).
// Used in t0 to mark packet origin: 0 = local port, 1 = arrived from Geneve.
func BuildActionsSetReg0(value uint32) []byte {
	a := make([]byte, 16)
	binary.BigEndian.PutUint16(a[0:2], ofpatSetField)
	binary.BigEndian.PutUint16(a[2:4], 16)
	copy(a[4:], OxmReg0(value))
	return a
}

// BuildActionsSetTunDst encodes OFPAT_SET_FIELD for NXM_NX_TUN_IPV4_DST.
// 4-byte action header + 4 OXM hdr + 4 IP = 12 bytes; pad to 16 (8-aligned).
func BuildActionsSetTunDst(ipBE uint32) []byte {
	a := make([]byte, 16)
	binary.BigEndian.PutUint16(a[0:2], ofpatSetField)
	binary.BigEndian.PutUint16(a[2:4], 16)
	copy(a[4:], OxmTunIPv4Dst(ipBE))
	return a
}

// BuildActionsSetMetadata encodes OFPAT_SET_FIELD for OXM_OF_METADATA.
// 4-byte action header + 4 OXM hdr + 8 value = 16 bytes (already 8-aligned).
func BuildActionsSetMetadata(value uint64) []byte {
	a := make([]byte, 16)
	binary.BigEndian.PutUint16(a[0:2], ofpatSetField)
	binary.BigEndian.PutUint16(a[2:4], 16)
	copy(a[4:], OxmMetadata(value))
	return a
}

// --- Nicira experimenter actions ---

// BuildActionsResubmitTable encodes NXAST_RESUBMIT_TABLE: re-run lookup in
// table `tableID` with the original in_port. Spec §7 uses this between
// pipeline stages (resubmit(,1), resubmit(,3), resubmit(,5), resubmit(,6)).
//
// Wire format: OFPAT_EXPERIMENTER(0xffff)/16 + vendor(4) + subtype(2) +
// in_port(2) + table_id(1) + pad(3) = 16 bytes.
func BuildActionsResubmitTable(tableID uint8) []byte {
	a := make([]byte, 16)
	binary.BigEndian.PutUint16(a[0:2], 0xffff) // OFPAT_EXPERIMENTER
	binary.BigEndian.PutUint16(a[2:4], 16)
	binary.BigEndian.PutUint32(a[4:8], NxVendorID)
	binary.BigEndian.PutUint16(a[8:10], nxastResubmitTable)
	binary.BigEndian.PutUint16(a[10:12], 0xfff8) // OFPP_IN_PORT
	a[12] = tableID
	// pad [13:16]
	return a
}

// BuildActionsRegMove encodes NXAST_REG_MOVE: copy `nBits` bits starting at
// srcOfs of srcOXM into dstOfs of dstOXM. OXM IDs are 4-byte headers (no
// value).
//
// Wire format: OFPAT_EXPERIMENTER/24 + vendor(4) + subtype(2) + n_bits(2) +
// src_ofs(2) + dst_ofs(2) + src_oxm_id(4) + dst_oxm_id(4) = 24 bytes.
func BuildActionsRegMove(nBits, srcOfs, dstOfs uint16, srcOXMID, dstOXMID uint32) []byte {
	a := make([]byte, 24)
	binary.BigEndian.PutUint16(a[0:2], 0xffff)
	binary.BigEndian.PutUint16(a[2:4], 24)
	binary.BigEndian.PutUint32(a[4:8], NxVendorID)
	binary.BigEndian.PutUint16(a[8:10], nxastRegMove)
	binary.BigEndian.PutUint16(a[10:12], nBits)
	binary.BigEndian.PutUint16(a[12:14], srcOfs)
	binary.BigEndian.PutUint16(a[14:16], dstOfs)
	binary.BigEndian.PutUint32(a[16:20], srcOXMID)
	binary.BigEndian.PutUint32(a[20:24], dstOXMID)
	return a
}

// BuildMatchAdvanced extends BuildMatch with metadata + reg0 + reg1 matches.
// Any zero-value parameter (matchMetadata=false / hasReg0=false / hasReg1=false)
// is omitted. inPort=0 means no in_port match.
func BuildMatchAdvanced(
	inPort uint32,
	metadata uint64,
	hasMetadata bool,
	reg0 uint32,
	hasReg0 bool,
	reg1 uint32,
	hasReg1 bool,
	tunID uint64,
	hasTunID bool,
) []byte {
	var fields []byte
	if inPort != 0 {
		fields = append(fields, OxmInPort(inPort)...)
	}
	if hasMetadata {
		fields = append(fields, OxmMetadata(metadata)...)
	}
	if hasReg0 {
		fields = append(fields, OxmReg0(reg0)...)
	}
	if hasReg1 {
		fields = append(fields, OxmReg1(reg1)...)
	}
	if hasTunID {
		fields = append(fields, OxmTunnelID(tunID)...)
	}
	rawLen := 4 + len(fields)
	padded := (rawLen + 7) &^ 7
	m := make([]byte, padded)
	binary.BigEndian.PutUint16(m[0:2], 1) // OFPMT_OXM
	binary.BigEndian.PutUint16(m[2:4], uint16(rawLen))
	copy(m[4:], fields)
	return m
}

// BuildMatchTunSrc is BuildMatch with the tunnel source address (NXM_NX_TUN_IPV4_SRC) added: in_port plus the IPv4 source of the
// outer header of a tunnelled packet. ip is the address in host byte order (as OxmTunIPv4Src takes it).
func BuildMatchTunSrc(inPortNo uint32, ip uint32) []byte {
	fields := append(OxmInPort(inPortNo), OxmTunIPv4Src(ip)...)
	rawLen := 4 + len(fields)
	padded := (rawLen + 7) &^ 7
	m := make([]byte, padded)
	binary.BigEndian.PutUint16(m[0:2], 1) // OFPMT_OXM
	binary.BigEndian.PutUint16(m[2:4], uint16(rawLen))
	copy(m[4:], fields)
	return m
}
