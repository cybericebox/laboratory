package demux

import "github.com/cybericebox/laboratory/internal/bandwidth"

// fastPathPacketBytes is the packet size the kernel fast path assumes when it turns a bandwidth ceiling into a packet rate: the largest
// WireGuard packet. The kernel programs count packets (one token bucket per session, both directions together), so a ceiling in bits
// becomes the packet rate that would carry that many bits at full size. A session that sends smaller packets is held to fewer bits
// than the ceiling, never to more. The class of the demux is not enforced in the kernel (only the userspace mode has it).
const fastPathPacketBytes = 1500

// PacketRateFor is the packets per second one session may send through the fast path, given the configured packet rate (0 = none)
// and the bandwidth limits: the smaller of the two, 0 when neither limits.
func PacketRateFor(base float64, l bandwidth.Limits) float64 {
	ceil := l.CapKbps
	if ceil <= 0 {
		ceil = l.ClassKbps // no per-participant ceiling: the whole class is the most one session could ever take
	}
	if ceil <= 0 {
		return base
	}
	derived := float64(ceil) * 125 / fastPathPacketBytes
	if base > 0 && base < derived {
		return base
	}
	return derived
}

// PacketBurstFor is the burst that goes with a packet rate: the configured one, or a tenth of a second of the rate if that is more
// (a burst of a few packets would pace a bulk transfer into a stall), at least 16 packets.
func PacketBurstFor(rate float64, burst int) int {
	if rate <= 0 {
		return burst
	}
	return max(burst, int(rate/10), 16)
}
