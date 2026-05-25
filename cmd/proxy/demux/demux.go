package demux

import (
	"encoding/binary"
	"fmt"
	"net"
)

const wgPort = 51820

// Demux handles incoming WireGuard UDP packets.
// Type 1 (handshake init) and Type 2 (handshake response) are handled in userspace.
// Type 4 (transport data) is handled by XDP when loaded; falls back to userspace.
type Demux struct {
	table     *Table
	conntrack *ConnTrack
	conn      *net.UDPConn
}

func New(listenAddr string, table *Table, ct *ConnTrack) (*Demux, error) {
	addr, err := net.ResolveUDPAddr("udp4", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve addr %s: %w", listenAddr, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP %s: %w", listenAddr, err)
	}
	return &Demux{table: table, conntrack: ct, conn: conn}, nil
}

// Run processes incoming WireGuard packets (type 1, 2, and 4 fallback).
func (d *Demux) Run(stop <-chan struct{}) {
	buf := make([]byte, 65535)
	for {
		select {
		case <-stop:
			d.conn.Close()
			return
		default:
		}
		n, src, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			// On stop, conn is closed and ReadFromUDP returns an error.
			select {
			case <-stop:
				return
			default:
				continue
			}
		}
		pkt := buf[:n]
		if len(pkt) < 1 {
			continue
		}
		switch pkt[0] {
		case 1:
			d.handleType1(pkt, src)
		case 2:
			d.handleType2(pkt, src)
		case 4:
			d.handleType4Userspace(pkt, src)
		}
	}
}

func (d *Demux) handleType1(pkt []byte, src *net.UDPAddr) {
	uid, found := d.table.FindByMac1(pkt)
	if !found {
		return
	}
	serverAddrStr := fmt.Sprintf("vpn.labgroup-%s.svc.cluster.local:%d", uid, wgPort)
	resolved, err := net.ResolveUDPAddr("udp4", serverAddrStr)
	if err != nil {
		return
	}
	serverConn, err := net.DialUDP("udp4", nil, resolved)
	if err != nil {
		return
	}
	_, _ = serverConn.Write(pkt)
	serverConn.Close()

	if len(pkt) < 8 {
		return
	}
	ci := binary.LittleEndian.Uint32(pkt[4:8])
	d.conntrack.AddPartial(ci,
		Socket{IP: src.IP, Port: uint16(src.Port)},
		Socket{IP: resolved.IP, Port: uint16(resolved.Port)})
}

func (d *Demux) handleType2(pkt []byte, src *net.UDPAddr) {
	if len(pkt) < 12 {
		return
	}
	si := binary.LittleEndian.Uint32(pkt[4:8])
	ci := binary.LittleEndian.Uint32(pkt[8:12])

	clientDst, found := d.conntrack.Lookup(ci)
	if !found {
		return
	}
	dst := &net.UDPAddr{IP: clientDst.IP, Port: int(clientDst.Port)}
	_, _ = d.conn.WriteToUDP(pkt, dst)

	serverSocket := Socket{IP: src.IP, Port: uint16(src.Port)}
	d.conntrack.Complete(si, ci, clientDst, serverSocket)
}

func (d *Demux) handleType4Userspace(pkt []byte, src *net.UDPAddr) {
	if len(pkt) < 8 {
		return
	}
	receiverIndex := binary.LittleEndian.Uint32(pkt[4:8])
	dst, found := d.conntrack.Lookup(receiverIndex)
	if !found {
		return
	}
	_, _ = d.conn.WriteToUDP(pkt, &net.UDPAddr{IP: dst.IP, Port: int(dst.Port)})
}

func (d *Demux) Close() {
	d.conn.Close()
}
