// cmd/proxy/demux/xdp/doc.go

// Package xdp contains the XDP BPF program that demultiplexes WireGuard
// transport-data packets (type 4) by receiver_index and rewrites IP/UDP/Ethernet
// headers to forward directly to the destination pod.
//
// Generated type names (for Task 2 / loader.go):
//   - WgDemuxObjects          – top-level container (WgDemuxMaps + WgDemuxPrograms)
//   - WgDemuxMaps.WgSessions  – *ebpf.Map, key=uint32 (receiver_index), value=WgDemuxDstEntry
//   - WgDemuxMaps.XdpCfgMap   – *ebpf.Map, key=uint32 (always 0), value=WgDemuxXdpCfg
//   - WgDemuxPrograms.WgDemux – *ebpf.Program (XDP)
//   - WgDemuxDstEntry         – map value: {Ip uint32, Port uint16, Pad uint16}
//   - WgDemuxXdpCfg           – map value: {ProxyIp uint32, ProxyPort uint16, GwMac [6]uint8, Pad [2]uint8}
//
// Note: bpf2go strips underscores from the C source name "wg_demux" when forming
// the output file prefix, producing wgdemux_bpfel.go / wgdemux_bpfeb.go.
package xdp

//go:generate sh -c "MULTIARCH=$(dpkg-architecture -q DEB_HOST_MULTIARCH 2>/dev/null || echo x86_64-linux-gnu) && go run github.com/cilium/ebpf/cmd/bpf2go -go-package xdp WgDemux ./wg_demux.bpf.c -- -target bpf -I/usr/include/$MULTIARCH -I/usr/include"
