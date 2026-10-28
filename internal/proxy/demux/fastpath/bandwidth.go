package fastpath

import "time"

// Class window: the node's class is counted in windows long enough to hold a few full-size packets at the class rate.
const (
	minClassWindow = time.Millisecond
	classMTU       = 1500
	// minSessionBurst lets a few full-size packets through a session's byte bucket.
	minSessionBurst = 16384
)

// limitValues are the numbers the programs read (struct wg_cfg).
type limitValues struct {
	byteRate, byteBurst   uint64 // per session: bytes per second and bytes; 0 = unlimited
	windowNs, windowBytes uint64 // the node's class: bytes in a window; 0 = unlimited
}

// byteLimits turns the rate of a session and of the class (kbit/s, 0 = none) into the numbers of the programs. The session's burst is a
// tenth of a second of its rate, at least a few full-size packets. The class is counted in windows: one millisecond, or longer when
// that would not hold two full-size packets at the class rate.
func byteLimits(sessionKbps, classKbps int64) limitValues {
	var l limitValues
	if sessionKbps > 0 {
		l.byteRate = uint64(sessionKbps) * 125
		l.byteBurst = max(l.byteRate/10, minSessionBurst)
	}
	if classKbps > 0 {
		rate := uint64(classKbps) * 125 // bytes per second
		l.windowNs = max(uint64(minClassWindow), 2*classMTU*uint64(time.Second)/rate)
		l.windowBytes = max(rate*l.windowNs/uint64(time.Second), 2*classMTU)
	}
	return l
}
