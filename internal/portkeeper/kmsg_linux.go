package portkeeper

import (
	"os"
	"sync"

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
// kernel.dmesg_restrict is 0. One read buffer is kept across polls.
type OSKernelLog struct {
	mu  sync.Mutex
	buf []byte
}

// Guest reads /proc/cmdline (the command line of the kernel, which a process in the device cannot change) and the printk.time
// parameter: the cursor of the follow needs timestamps.
func (*OSKernelLog) Guest() (bool, string) {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false, "the kernel command line cannot be read"
	}
	if ok, reason := kataGuest(string(b)); !ok {
		return false, reason
	}
	t, err := os.ReadFile("/sys/module/printk/parameters/time")
	if err != nil || !printkTimeOn(string(t)) {
		return false, "the kernel messages carry no timestamps (printk.time)"
	}
	return true, ""
}

// ReadAll is syslog(SYSLOG_ACTION_READ_ALL) into a buffer of the ring buffer's size. The result is valid until the next call.
func (o *OSKernelLog) ReadAll() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	size, err := unix.Klogctl(syslogActionSizeBuffer, nil)
	if err != nil {
		return nil, err
	}
	n := min(max(size, kmsgReadMin), kmsgReadMax)
	if cap(o.buf) < n {
		o.buf = make([]byte, n)
	}
	got, err := unix.Klogctl(syslogActionReadAll, o.buf[:n])
	if err != nil {
		return nil, err
	}
	return o.buf[:got], nil
}
