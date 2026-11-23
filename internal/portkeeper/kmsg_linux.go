package portkeeper

import (
	"os"

	"golang.org/x/sys/unix"
)

// syslog(2) actions (linux/include/linux/syslog.h); golang.org/x/sys has no names for them.
const (
	syslogActionReadAll    = 3
	syslogActionSizeBuffer = 10
	// kmsgReadMin and kmsgReadMax bound the read buffer: READ_ALL returns the newest messages that fit.
	kmsgReadMin = 16 << 10
	kmsgReadMax = 4 << 20
)

// OSKernelLog is the kernel log of the kernel the keeper runs on, through syslog(2): no device node and no capability while
// kernel.dmesg_restrict is 0.
type OSKernelLog struct{}

// Guest reads /proc/cmdline: the command line of the kernel, which a process in the device cannot change.
func (OSKernelLog) Guest() (bool, string) {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false, "the kernel command line cannot be read"
	}
	return kataGuest(string(b))
}

// ReadAll is syslog(SYSLOG_ACTION_READ_ALL) into a buffer of the ring buffer's size.
func (OSKernelLog) ReadAll() ([]byte, error) {
	size, err := unix.Klogctl(syslogActionSizeBuffer, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, min(max(size, kmsgReadMin), kmsgReadMax))
	n, err := unix.Klogctl(syslogActionReadAll, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
