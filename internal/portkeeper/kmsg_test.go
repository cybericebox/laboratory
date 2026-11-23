package portkeeper

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKernelLog is a ring buffer as syslog(2) READ_ALL returns it, with a guest answer; it counts the reads.
type fakeKernelLog struct {
	mu     sync.Mutex
	lines  []string
	guest  bool
	err    error
	reads  int
	guests int
}

func (f *fakeKernelLog) Guest() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.guests++
	if !f.guest {
		return false, "not a Kata guest"
	}
	return true, ""
}

func (f *fakeKernelLog) ReadAll() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	return []byte(strings.Join(f.lines, "\n") + "\n"), nil
}

func (f *fakeKernelLog) add(l ...string) {
	f.mu.Lock()
	f.lines = append(f.lines, l...)
	f.mu.Unlock()
}

func (f *fakeKernelLog) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func bootLog() *fakeKernelLog {
	return &fakeKernelLog{guest: true, lines: []string{
		"<5>[    0.000000] Linux version 6.12.47 (kata) #1 SMP",
		"<6>[    0.000000] Command line: reboot=k panic=1 systemd.unit=kata-containers.target",
		"<6>[    0.120000] virtio_net virtio1 eth0: renamed",
		"<6>[    1.500000] EXT4-fs (vda): mounted",
	}}
}

// The dump is every line of the ring buffer, without the level prefix, each ended with CR LF (the line is raw, nothing adds the CR).
func TestKernelLogDumpIsTheWholeRingBuffer(t *testing.T) {
	f := newKmsgFollower(bootLog())
	got := string(f.dump())
	want := "[    0.000000] Linux version 6.12.47 (kata) #1 SMP\r\n" +
		"[    0.000000] Command line: reboot=k panic=1 systemd.unit=kata-containers.target\r\n" +
		"[    0.120000] virtio_net virtio1 eth0: renamed\r\n" +
		"[    1.500000] EXT4-fs (vda): mounted\r\n"
	if got != want {
		t.Fatalf("dump:\n%q\nwant\n%q", got, want)
	}
	if b := f.poll(); len(b) != 0 {
		t.Fatalf("nothing new after a dump: %q", b)
	}
}

// The follow writes only what came after the last line it wrote, also for lines with the same timestamp and lines without one.
func TestKernelLogFollowsOnlyNewLines(t *testing.T) {
	src := bootLog()
	f := newKmsgFollower(src)
	_ = f.dump()
	src.add("<6>[    1.500000] second line of the same microsecond", "<4>[    7.000001] nft: LOG IN=eth0", "continuation without a prefix")
	got := string(f.poll())
	want := "[    1.500000] second line of the same microsecond\r\n[    7.000001] nft: LOG IN=eth0\r\ncontinuation without a prefix\r\n"
	if got != want {
		t.Fatalf("poll %q, want %q", got, want)
	}
	if b := f.poll(); len(b) != 0 {
		t.Fatalf("a second poll repeats: %q", b)
	}
	// the ring buffer wrapped: the oldest lines are gone, the new one still comes once
	src.mu.Lock()
	src.lines = append([]string(nil), src.lines[4:]...)
	src.mu.Unlock()
	src.add("<6>[    9.000000] after the wrap")
	if got := string(f.poll()); got != "[    9.000000] after the wrap\r\n" {
		t.Fatalf("after a wrap: %q", got)
	}
}

// A kernel message cannot put a control sequence on the line (the other end is a terminal of another device): every control byte
// but the tab, and every byte that is not UTF-8, becomes '?'; a very long line is cut.
func TestKernelLogLinesAreSanitized(t *testing.T) {
	src := &fakeKernelLog{guest: true, lines: []string{
		"<4>[    2.000000] evil \x1b]0;title\x07 \x1b[2J\tend\x9b\xc2\x9b\x7f ok é",
		"<4>[    3.000000] " + strings.Repeat("x", 3000),
	}}
	out := strings.Split(string(newKmsgFollower(src).dump()), "\r\n")
	if out[0] != "[    2.000000] evil ?]0;title? ?[2J\tend??? ok é" {
		t.Fatalf("sanitized %q", out[0])
	}
	if len(out[1]) > kmsgLineMax+len("…") {
		t.Fatalf("a long line is cut: %d bytes", len(out[1]))
	}
}

// A flood of kernel messages (a netfilter LOG rule) never takes the line: one poll writes at most kmsgFollowMax bytes and says how
// many messages it left out; the dump keeps the newest kmsgDumpMax bytes and says how many earlier ones it left out.
func TestKernelLogFloodIsBounded(t *testing.T) {
	src := bootLog()
	f := newKmsgFollower(src)
	_ = f.dump()
	for i := 0; i < 5000; i++ {
		src.add(fmt.Sprintf("<4>[   10.%06d] nft LOG: IN=eth0 OUT= SRC=10.0.0.%d DST=10.0.0.1 LEN=60 PROTO=TCP SPT=4444 DPT=22", i, i%250))
	}
	got := string(f.poll())
	if len(got) > kmsgFollowMax+128 || !strings.Contains(got, "kernel log:") || !strings.Contains(got, "messages left out") {
		t.Fatalf("poll of a flood: %d bytes, tail %q", len(got), got[max(0, len(got)-200):])
	}
	if !strings.HasPrefix(got, "[   10.000000]") {
		t.Fatalf("the follow keeps the order: %q", got[:40])
	}
	if b := f.poll(); len(b) != 0 {
		t.Fatalf("the left out messages never come later: %d bytes", len(b))
	}
	d := string(f.dump())
	if len(d) > kmsgDumpMax+128 || !strings.HasPrefix(d, "[kernel log: ") || !strings.Contains(d, "earlier messages left out") ||
		!strings.HasSuffix(d, "SRC=10.0.0.249 DST=10.0.0.1 LEN=60 PROTO=TCP SPT=4444 DPT=22\r\n") {
		t.Fatalf("dump of a flood: %d bytes, head %q", len(d), d[:min(len(d), 80)])
	}
}

// Outside a Kata guest the kernel is the node's: the keeper never reads it, and a client sees why there is no kernel log.
func TestKernelLogNeverReadOutsideAGuest(t *testing.T) {
	src := bootLog()
	src.guest = false
	f := newKmsgFollower(src)
	for range 3 {
		if got := string(f.dump()); !strings.Contains(got, "kernel log not available") || strings.Contains(got, "Linux version") {
			t.Fatalf("dump outside a guest: %q", got)
		}
		if b := f.poll(); len(b) != 0 {
			t.Fatalf("poll outside a guest: %q", b)
		}
	}
	if src.readCount() != 0 {
		t.Fatalf("the kernel log was read %d times outside a guest", src.readCount())
	}
}

// A read that fails (EPERM: dmesg_restrict, or a seccomp profile in the guest) turns the kernel log off for good: one notice, no retry
// loop.
func TestKernelLogOffAfterAFailedRead(t *testing.T) {
	src := bootLog()
	src.err = errors.New("operation not permitted")
	f := newKmsgFollower(src)
	if got := string(f.dump()); !strings.Contains(got, "kernel log not available: operation not permitted") {
		t.Fatalf("dump %q", got)
	}
	n := src.readCount()
	for range 5 {
		_ = f.poll()
		_ = f.dump()
	}
	if src.readCount() != n {
		t.Fatalf("read again after a failure: %d -> %d", n, src.readCount())
	}
}

func TestKataGuestCommandLine(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"tsc=reliable no_timer_check rcupdate.rcu_expedited=1 reboot=k panic=1 systemd.unit=kata-containers.target systemd.mask=systemd-networkd.service quiet\n": true,
		"BOOT_IMAGE=/vmlinuz-6.8.0-45-generic root=UUID=1 ro quiet splash\n":                                                                                      false,
		"systemd.unit=kata-containers.target.wants":                                                                                                               false,
		"": false,
	} {
		if got, _ := kataGuest(cmdline); got != want {
			t.Errorf("%q: got %v", cmdline, got)
		}
	}
}

// Through the keeper: a client that attaches to a kernel log line gets the ring buffer, then the new messages; a line without the flag
// never touches the kernel log.
func TestKeeperWritesTheKernelLogToTheLine(t *testing.T) {
	src := bootLog()
	spec := Spec{Console: []ConsolePort{{Name: "console0", KernelLog: true}, {Name: "aux0"}}}
	_, sockets, _, _ := startKeeper(t, spec, func(s *fakeSys) { s.klog = src })
	c := dialAttached(t, SocketPath(sockets, "console0"))
	defer func() { _ = c.Close() }()
	readUntil := func(want string) {
		t.Helper()
		var got strings.Builder
		buf := make([]byte, 4096)
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		for !strings.Contains(got.String(), want) {
			n, err := c.Read(buf)
			got.Write(buf[:n])
			if err != nil {
				t.Fatalf("waiting for %q: %v (got %q)", want, err, got.String())
			}
		}
	}
	readUntil("EXT4-fs (vda): mounted\r\n")
	src.add("<3>[   42.000000] Out of memory: Killed process 77 (stress)")
	readUntil("[   42.000000] Out of memory: Killed process 77 (stress)\r\n")

	reads := src.readCount()
	a := dialAttached(t, SocketPath(sockets, "aux0"))
	_ = a.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := a.Read(make([]byte, 64)); n != 0 || !isTimeout(err) {
		t.Fatalf("a line without the kernel log got %d bytes (%v)", n, err)
	}
	_ = a.Close()
	if src.readCount() < reads {
		t.Fatal("reads went backwards")
	}
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// A keeper whose list has no kernel log port never asks whether it runs in a guest and never reads the kernel log.
func TestKeeperWithoutKernelLogNeverReadsIt(t *testing.T) {
	src := bootLog()
	_, sockets, _, _ := startKeeper(t, Spec{Console: []ConsolePort{{Name: "console0"}}}, func(s *fakeSys) { s.klog = src })
	c := dialAttached(t, SocketPath(sockets, "console0"))
	time.Sleep(100 * time.Millisecond)
	_ = c.Close()
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.reads != 0 || src.guests != 0 {
		t.Fatalf("reads %d guest checks %d", src.reads, src.guests)
	}
}
