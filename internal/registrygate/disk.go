package registrygate

import (
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DiskWatch says whether the volume is above its high-water mark (percent of its blocks in use). The answer is kept for a few seconds:
// it is asked on every miss.
type DiskWatch struct {
	Path      string
	HighWater int // percent, 1..100; 0 = never full
	TTL       time.Duration
	Now       func() time.Time

	mu   sync.Mutex
	at   time.Time
	full bool
}

// Full says whether a miss must be streamed through unstored. A volume that cannot be measured is not treated as full.
func (d *DiskWatch) Full() bool {
	if d.HighWater <= 0 {
		return false
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ttl := d.TTL
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	if !d.at.IsZero() && now().Sub(d.at) < ttl {
		return d.full
	}
	d.at = now()
	var st unix.Statfs_t
	if err := unix.Statfs(d.Path, &st); err != nil || st.Blocks == 0 {
		d.full = false
		return false
	}
	used := (st.Blocks - st.Bavail) * 100 / st.Blocks
	d.full = used >= uint64(d.HighWater)
	return d.full
}
