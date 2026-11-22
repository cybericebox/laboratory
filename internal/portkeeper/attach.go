package portkeeper

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Exit codes of probe port-attach (the device access service tells them apart; phase 6 of the design acts on them).
const (
	// ExitUsage: the port name is not a port name.
	ExitUsage = 2
	// ExitBusy: the line has a client already (one line, one cable).
	ExitBusy = 3
	// ExitNoKeeper: no keeper serves the port, and none could be started (the device has no such port, or the keeper does not start).
	ExitNoKeeper = 4
	// ExitPortFailed: the keeper runs but could not make this port; the reason is written to stderr.
	ExitPortFailed = 5
)

// attachRetry is how long an attach waits for a keeper it started to serve the port.
const attachRetry = 3 * time.Second

// Attach is probe port-attach: it connects to the keeper's socket of the port and copies bytes between it and stdin/stdout (the FIFOs
// of the device access service's exec; Terminal false, so no line discipline is in between) until either side ends. When nobody
// serves the socket it calls start once (a keeper killed by root in the device is started again: its lines restart) and retries.
func Attach(dir, port string, stdin io.Reader, stdout io.Writer, start func() error) int {
	return attach(dir, port, stdin, stdout, io.Discard, start)
}

func attach(dir, port string, stdin io.Reader, stdout, stderr io.Writer, start func() error) int {
	if !ValidPortName(port) {
		return ExitUsage
	}
	sock := SocketPath(dir, port)
	// Dial; when nobody answers, start a keeper and dial again; only then is a record of a failed port the answer (a record left by a
	// keeper that is gone says nothing about the next one).
	conn, err := net.Dial("unix", sock)
	if err != nil && start != nil && start() == nil {
		for deadline := time.Now().Add(attachRetry); err != nil && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
			conn, err = net.Dial("unix", sock)
		}
	}
	if err != nil {
		if reason, rerr := os.ReadFile(filepath.Join(dir, port+failedSuffix)); rerr == nil {
			_, _ = fmt.Fprintf(stderr, "port %s: %s\n", port, reason)
			return ExitPortFailed
		}
		return ExitNoKeeper
	}
	defer func() { _ = conn.Close() }()
	ack := make([]byte, 1)
	if _, err := io.ReadFull(conn, ack); err != nil || ack[0] != AttachOK {
		return ExitBusy
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, stdin)
		if c, ok := conn.(*net.UnixConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(stdout, conn)
		done <- struct{}{}
	}()
	<-done
	return 0
}
