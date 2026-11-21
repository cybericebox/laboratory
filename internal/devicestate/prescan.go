package devicestate

import (
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// DefaultPrescanTime is how long the pre-scan of a layer may take before the layer is refused as too big to look at.
const DefaultPrescanTime = 20 * time.Second

// prescanLayer is the bounded look at a writable layer before containerd is asked to diff it. The layer is a participant's file system:
// without this, containerd would walk and tar all of it (millions of entries, a sparse file of terabytes) before the first bound applies,
// and that work is the node's. The scan goes through an os.Root on the layer directory, reads the directories in batches, counts what the
// snapshot filter would keep (the entries, the apparent size of the regular files that are not over the file size limit) and stops at the
// entry cap, at the write quota, and at the time cap. It returns the errors the filter returns for the same layer (snapshot.ErrEntries,
// snapshot.ErrQuota), so the caller refuses and holds the layer alike, without a diff. A layer that fits is not this function's to judge
// further: the filter after the diff stays the final bound.
func prescanLayer(upper string, pol snapshot.Policy, limit time.Duration) error {
	if limit <= 0 {
		limit = DefaultPrescanTime
	}
	rt, err := os.OpenRoot(upper)
	if err != nil {
		return nil // no layer to look at: the diff reports what is wrong
	}
	defer func() { _ = rt.Close() }()
	var (
		entries int
		bytes   int64
		refused error
	)
	// The walk itself is bounded by what could still be a layer that fits: the filter drops some entries without counting them, so the
	// look-at cap is a multiple of the entry cap, never unlimited.
	lookCap := 1 << 22
	if pol.MaxEntries > 0 && 4*pol.MaxEntries+1024 < lookCap {
		lookCap = 4*pol.MaxEntries + 1024
	}
	over := scanTree(rt, ".", pol, scanLimits{entries: lookCap, time: limit}, func(p string, d fs.DirEntry) (bool, bool) {
		fi, err := d.Info()
		if err != nil {
			return d.IsDir(), false // gone since it was listed
		}
		switch m := fi.Mode(); {
		case m.IsDir(), m&fs.ModeSymlink != 0:
			entries++
		case m.IsRegular():
			if pol.MaxFileSize > 0 && fi.Size() > pol.MaxFileSize {
				return false, false // left out for its size, not counted
			}
			entries++
			bytes += fi.Size() // the apparent size: a sparse file counts what it claims, which is what a tar of it would stream
			if pol.WriteQuota > 0 && bytes > pol.WriteQuota {
				refused = fmt.Errorf("%w: the layer passes the quota of %d bytes", snapshot.ErrQuota, pol.WriteQuota)
				return false, true
			}
		case m&fs.ModeCharDevice != 0:
			entries++ // overlayfs deletions are 0:0 character devices, written as whiteouts
		}
		if pol.MaxEntries > 0 && entries > pol.MaxEntries {
			refused = fmt.Errorf("%w: more than %d entries", snapshot.ErrEntries, pol.MaxEntries)
			return false, true
		}
		return d.IsDir(), false
	})
	if refused != nil {
		return refused
	}
	if over {
		return fmt.Errorf("%w: the layer has too many entries to look at within %s", snapshot.ErrEntries, limit)
	}
	return nil
}
