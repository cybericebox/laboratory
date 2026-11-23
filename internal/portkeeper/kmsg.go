package portkeeper

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The kernel log on a console line (ConsolePort.KernelLog; docs/internal-docs/laboratory/research/2026-11-23-kata-serial-console.md,
// step 1). A microVM device has a kernel of its own, so its console line may show what a real console shows besides the login prompt:
// when a client attaches, the keeper writes the kernel's ring buffer (every message since boot), then it follows new messages while the
// client stays. The bytes go towards the client only (never to the program on the line), and the program's own output is not changed.
//
// How the log is read: syslog(2) SYSLOG_ACTION_READ_ALL, polled. /dev/kmsg is not used: a container's /dev has only the OCI default
// devices and the device cgroup does not allow 1:11, so it would need mknod, a device rule and a capability. READ_ALL needs no
// capability while kernel.dmesg_restrict is 0 (the Kata guest kernel does not set CONFIG_SECURITY_DMESG_RESTRICT, and kernel/microvm/
// build.sh checks it); it does not block, so the keeper polls and keeps a cursor (timestamp, position among the lines of that timestamp).
//
// The node's kernel is never read. The operator sets the flag only for a microvm device (consolespec.KeeperSpec), and the keeper checks
// again before the first read that the kernel command line is a Kata guest's (KernelLog.Guest). A failed read (EPERM: dmesg_restrict, or a
// seccomp profile applied in the guest) turns the kernel log of the line off for good: a client sees one notice, nothing retries.

// KernelLog is where the keeper reads the kernel's log (OSKernelLog in a device; a test gives its own).
type KernelLog interface {
	// Guest says the keeper runs on the guest kernel of a Kata microVM, not on a node's kernel; reason says why not.
	Guest() (ok bool, reason string)
	// ReadAll is the ring buffer as syslog(2) READ_ALL gives it: lines "<level>[seconds.micros] text".
	ReadAll() ([]byte, error)
}

const (
	// KernelLogPoll is how often the keeper looks for new kernel messages while a client is attached.
	KernelLogPoll = 500 * time.Millisecond
	// kmsgFollowMax bounds what one poll writes (16 KiB/s at the default poll, a quarter of a line's 64 KiB/s): the rest of a burst is
	// left out and counted, so a flood of kernel messages (a netfilter LOG rule) never takes the line from the login.
	kmsgFollowMax = 8 << 10
	// kmsgDumpMax bounds the ring buffer written on attach: the newest messages that fit, with a count of the ones left out.
	kmsgDumpMax = 64 << 10
	// kmsgLineMax cuts a longer message (the kernel's own records are at most 1 KiB).
	kmsgLineMax = 1024
	// kataGuestUnit is on the kernel command line of every Kata guest whose image runs systemd as PID 1 (the default guest image): Kata
	// starts kata-containers.target on it. A node's command line never has it.
	kataGuestUnit = "systemd.unit=kata-containers.target"
)

// printkTimeOn says the printk.time parameter (/sys/module/printk/parameters/time) is on: the keeper's cursor is the timestamp of each
// message, so without timestamps it would lose messages without a word once the ring buffer wraps. "N" (printk.time=0) turns the kernel
// log of the line off with the notice; an unreadable value is taken as off too.
func printkTimeOn(v string) bool {
	v = strings.TrimSpace(v)
	return v == "Y" || v == "1"
}

// kataGuest says the kernel command line is a Kata guest's.
func kataGuest(cmdline string) (bool, string) {
	for _, f := range strings.Fields(cmdline) {
		if f == kataGuestUnit {
			return true, ""
		}
	}
	return false, "the kernel is not the guest kernel of a microVM"
}

// kmsgLine is one line of the ring buffer: its timestamp in microseconds (the previous line's when it has none) and its text without
// the level prefix.
type kmsgLine struct {
	ts   int64
	text string
}

// kmsgKey orders the lines: the timestamp, then the position among the lines of the same timestamp in that read.
type kmsgKey struct {
	ts int64
	n  int
}

func (k kmsgKey) after(o kmsgKey) bool { return k.ts > o.ts || (k.ts == o.ts && k.n > o.n) }

// parseKmsg splits a READ_ALL buffer into lines with their keys.
func parseKmsg(b []byte) ([]kmsgLine, []kmsgKey) {
	var lines []kmsgLine
	var keys []kmsgKey
	ts, n := int64(0), -1
	for _, raw := range strings.Split(string(b), "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if raw == "" {
			continue
		}
		text := stripLevel(raw)
		if t, ok := timestamp(text); ok {
			if t != ts {
				ts, n = t, -1
			}
		}
		n++
		lines = append(lines, kmsgLine{ts: ts, text: text})
		keys = append(keys, kmsgKey{ts: ts, n: n})
	}
	return lines, keys
}

// stripLevel drops the "<level>" prefix of a record.
func stripLevel(s string) string {
	if len(s) < 3 || s[0] != '<' {
		return s
	}
	end := strings.IndexByte(s, '>')
	if end < 2 || end > 4 {
		return s
	}
	if _, err := strconv.Atoi(s[1:end]); err != nil {
		return s
	}
	return s[end+1:]
}

// timestamp reads "[ seconds.micros]" at the start of a line (CONFIG_PRINTK_TIME).
func timestamp(s string) (int64, bool) {
	if len(s) < 3 || s[0] != '[' {
		return 0, false
	}
	end := strings.IndexByte(s, ']')
	if end < 0 || end > 24 {
		return 0, false
	}
	secs, frac, ok := strings.Cut(strings.TrimSpace(s[1:end]), ".")
	if !ok || len(frac) == 0 || len(frac) > 6 {
		return 0, false
	}
	sec, err1 := strconv.ParseInt(secs, 10, 64)
	us, err2 := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	if err1 != nil || err2 != nil || sec < 0 || us < 0 {
		return 0, false
	}
	return sec*1_000_000 + us, true
}

// lineOut is a line as it goes on the wire: control bytes (but the tab) and bytes that are not UTF-8 become '?', so a message cannot
// send an escape sequence to the terminal at the other end; cut at kmsgLineMax; ended with CR LF (the line is raw).
func lineOut(text string) []byte {
	out := make([]byte, 0, min(len(text), kmsgLineMax)+5)
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if len(out)+size > kmsgLineMax {
			out = append(out, "…"...)
			break
		}
		switch {
		case r == utf8.RuneError && size <= 1, r < 0x20 && r != '\t', r >= 0x7f && r <= 0x9f,
			// format characters (bidi overrides and isolates, zero-width joiners and spaces) and the line and paragraph separators can
			// make a terminal show something other than what was sent
			unicode.Is(unicode.Cf, r), r == '\u2028', r == '\u2029':
			out = append(out, '?')
		default:
			out = append(out, text[i:i+size]...)
		}
		i += size
	}
	return append(out, '\r', '\n')
}

// kmsgFollower holds the cursor of one line: the key of the last kernel message it wrote (or skipped).
type kmsgFollower struct {
	src     KernelLog
	checked bool
	// off is why the line has no kernel log (not a guest, a failed read); empty while it works.
	off string
	// told: the notice of off was already given by a poll (a dump gives it again to each new client).
	told   bool
	cursor kmsgKey
	seen   bool
	// lastLen and lastTail are the size and the last bytes of the previous read: an unchanged buffer has nothing new, so an idle poll
	// is not parsed.
	lastLen  int
	lastTail string
}

func newKmsgFollower(src KernelLog) *kmsgFollower { return &kmsgFollower{src: src} }

// read is the ring buffer, after the guest check of the first call; nil when the kernel log is off.
func (f *kmsgFollower) read() ([]kmsgLine, []kmsgKey) {
	if !f.checked {
		f.checked = true
		if ok, reason := f.src.Guest(); !ok {
			f.off = reason
		}
	}
	if f.off != "" {
		return nil, nil
	}
	b, err := f.src.ReadAll()
	if err != nil {
		f.off = err.Error()
		return nil, nil
	}
	f.lastLen, f.lastTail = len(b), string(b[max(0, len(b)-kmsgTail):])
	return parseKmsg(b)
}

// kmsgTail is how much of the end of a read is kept to see that nothing changed.
const kmsgTail = 256

// unchanged reads the buffer and says it is the same as at the previous read (a cheap idle poll); it never moves the cursor.
func (f *kmsgFollower) unchanged() (bool, []kmsgLine, []kmsgKey) {
	b, err := f.src.ReadAll()
	if err != nil {
		f.off = err.Error()
		return false, nil, nil
	}
	if len(b) == f.lastLen && string(b[max(0, len(b)-kmsgTail):]) == f.lastTail {
		return true, nil, nil
	}
	f.lastLen, f.lastTail = len(b), string(b[max(0, len(b)-kmsgTail):])
	lines, keys := parseKmsg(b)
	return false, lines, keys
}

func (f *kmsgFollower) notice() []byte {
	return lineOut("[kernel log not available: " + f.off + "]")
}

// dump is the whole ring buffer (the newest kmsgDumpMax bytes) for a client that just attached; it moves the cursor to its end.
func (f *kmsgFollower) dump() []byte {
	lines, keys := f.read()
	if f.off != "" {
		return f.notice()
	}
	if len(lines) == 0 {
		return nil
	}
	f.cursor, f.seen = keys[len(keys)-1], true
	parts := make([][]byte, len(lines))
	total, first := 0, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		p := lineOut(lines[i].text)
		if total+len(p) > kmsgDumpMax {
			break
		}
		parts[i], total, first = p, total+len(p), i
	}
	out := make([]byte, 0, total+64)
	if first > 0 {
		out = append(out, lineOut(fmt.Sprintf("[kernel log: %d earlier messages left out]", first))...)
	}
	for _, p := range parts[first:] {
		out = append(out, p...)
	}
	return out
}

// poll is what came after the cursor, at most kmsgFollowMax bytes; the rest of a burst is left out and counted. It moves the cursor
// past everything it read, written or not.
func (f *kmsgFollower) poll() []byte {
	if f.off != "" {
		return nil
	}
	if !f.seen {
		// only a dump starts the follow: a poll before it would write a part of the ring buffer
		_ = f.dump()
		return nil
	}
	same, lines, keys := f.unchanged()
	if same {
		return nil
	}
	if f.off != "" {
		if f.told {
			return nil
		}
		f.told = true
		return f.notice()
	}
	if len(lines) == 0 {
		return nil
	}
	var out []byte
	dropped := 0
	for i := range lines {
		if f.seen && !keys[i].after(f.cursor) {
			continue
		}
		p := lineOut(lines[i].text)
		if dropped > 0 || len(out)+len(p) > kmsgFollowMax {
			dropped++
			continue
		}
		out = append(out, p...)
	}
	f.cursor, f.seen = keys[len(keys)-1], true
	if dropped > 0 {
		out = append(out, lineOut(fmt.Sprintf("[kernel log: %d messages left out, more than %d bytes at once]", dropped, kmsgFollowMax))...)
	}
	return out
}

// followKernelLog writes the kernel log to the client of h: the ring buffer when a client attaches, then new messages every poll while
// it stays. With no client nothing is read. attached is h.AttachEvents(), taken by the caller before the line is served, so no attach
// is missed.
func followKernelLog(ctx context.Context, h *Hub, attached <-chan struct{}, src KernelLog, every time.Duration) {
	if every <= 0 {
		every = KernelLogPoll
	}
	f := newKmsgFollower(src)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-attached:
			h.Inject(f.dump())
		case <-t.C:
			if !h.Attached() {
				continue
			}
			// A client that attached since the last pass gets its dump first, never a poll before it: its attach signalled the channel
			// under the same lock that Attached reads, so the signal is there by now.
			select {
			case <-attached:
				h.Inject(f.dump())
			default:
				h.Inject(f.poll())
			}
		}
	}
}
