//go:build linux

package devicestate

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// AcquireCaptureOwner serializes startup recovery, API invalidation and thaw
// across node-agent processes. Hold it until all capture workers have exited.
func AcquireCaptureOwner(workDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(workDir, "capture-owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another node-agent owns required capture holds: %w", err)
	}
	return f, nil
}
