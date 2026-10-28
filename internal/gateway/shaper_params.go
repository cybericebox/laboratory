package gateway

import "time"

// The shaping of one lab interface of the gateway pod, as numbers. Traffic of the lab towards the internet is policed where it enters
// the pod (the ingress of the lab interface: a token bucket that drops what is over), traffic from the internet to the lab is shaped
// where it leaves (the egress of the lab interface: a token bucket filter with a short queue). Both are one lab each, so no
// classification is needed, and both count bytes before and after the NAT alike.
const (
	// shapeBurstTime is how much traffic at the full rate may go at once.
	shapeBurstTime = 100 * time.Millisecond
	// shapeMinBurst covers a segmentation-offloaded frame (64 kB) with room to spare.
	shapeMinBurst = 128 * 1024
	// policeBurstTime and policeMinBurst are the burst of the police on the ingress. A police does not queue, it drops what is over: with
	// a burst of a tenth of a second TCP with large segments reached under half of the ceiling (measured: 9.6 of 20 Mbit/s), with
	// 1 MB it reaches 19.4. The ceiling still holds over any longer time.
	policeBurstTime = 400 * time.Millisecond
	policeMinBurst  = 1 << 20
	// shapeQueueTime is the longest the shaped direction queues a packet before it is dropped.
	shapeQueueTime = 50 * time.Millisecond
	// shapeMTU is the largest frame the police accepts (a segmentation-offloaded one).
	shapeMTU = 65535
)

// shapeParams turns a ceiling in kbit/s into the numbers the kernel takes: the rate in bytes per second, the burst in bytes, and the
// queue limit of the shaped direction in bytes, and the burst of the police.
func shapeParams(kbps int64) (rate uint64, burst, limit, policeBurst uint32) {
	rate = uint64(kbps) * 125
	b := max(uint64(float64(rate)*shapeBurstTime.Seconds()), shapeMinBurst)
	l := b + uint64(float64(rate)*shapeQueueTime.Seconds())
	pb := max(uint64(float64(rate)*policeBurstTime.Seconds()), policeMinBurst)
	return rate, uint32(min(b, 1<<31)), uint32(min(l, 1<<31)), uint32(min(pb, 1<<31))
}
