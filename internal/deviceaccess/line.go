package deviceaccess

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/portkeeper"
)

// Console lines (docs/internal-docs/laboratory/specs/console-ports-design.md, 6.3 and 6.5). A line is the two PTYs of one ConsoleCable,
// held by the port keepers of two devices, and the relay between them. The service attaches an end by running "probe port-attach <port>"
// in the device (root, no terminal: raw bytes) through the same containerd exec path as a console, in a device that Resolve's white list
// accepts and whose pod carries the lab's UID and the digest of the port list the node-agent sent. The node-agent of the console end's
// node makes the service the line owner: it attaches the console end and either the serial end too (same node: the bytes are copied in
// this process) or dials the service of the serial end's node (the cable stream, cable_stream.go). Each direction is capped at
// names.ConsoleLineMaxBytesPerSecond and blocks when the other side does not take bytes.

// MaxLinesPerNode bounds the lines of one node (design 6.7, point 8).
const MaxLinesPerNode = 500

// Retry of an owner whose line is down: from lineRetryMin, doubling, to lineRetryMax; back to the minimum after a line that stayed up for
// lineStable. Variables, so the tests run fast.
var (
	lineRetryMin = 500 * time.Millisecond
	lineRetryMax = 10 * time.Second
	lineStable   = 10 * time.Second
	// attachWait bounds the time from the start of the attach to its AttachOK (the attach itself may start a keeper and retry 3 s).
	attachWait = 10 * time.Second
)

// lineChunk is the most bytes one read of a side returns; lineBurst is the burst of the rate cap.
const (
	lineChunk = 4 << 10
	// lineBurst is at least the largest frame the stream gives (wire.MaxData); the relay also never waits for more than it at once.
	lineBurst = wire.MaxData
)

// errAttachRefused is an attach that ended before the keeper accepted it (busy, no keeper, the port failed).
var errAttachRefused = errors.New("the port could not be attached")

// side is one end of a relay: an attach to a keeper, or the cable stream to the other node.
type side interface {
	// read returns the next bytes of the side; an error ends the relay.
	read() ([]byte, error)
	// write gives bytes to the side; it blocks while the side does not take them, at most the write timeout.
	write(p []byte) error
	// close ends the side and unblocks read and write; reason is told to a stream peer.
	close(reason string)
}

// portAttach is a running "probe port-attach" in a device.
type portAttach struct {
	svc   *Service
	ex    Exec
	stdin *pipeWriter
	out   *io.PipeReader
	buf   []byte
	once  sync.Once
}

func (p *portAttach) read() ([]byte, error) {
	n, err := p.out.Read(p.buf)
	if n > 0 {
		return p.buf[:n], nil
	}
	if err == nil {
		err = io.EOF
	}
	return nil, err
}

func (p *portAttach) write(b []byte) error { return p.stdin.Write(b) }

func (p *portAttach) close(string) {
	p.once.Do(func() {
		p.stdin.Abort()
		_ = p.out.CloseWithError(io.ErrClosedPipe)
		ctx, cancel := context.WithTimeout(context.Background(), p.svc.lim.Cleanup)
		defer cancel()
		if err := p.ex.Close(ctx); err != nil {
			p.svc.log.Error(err, "clean up the port attach exec")
		}
	})
}

// endRef is what attachPort needs of an end.
type endRef struct {
	Namespace, Lab, LabUID string
	End                    wire.LineEnd
	Serial                 bool
}

// attachPort attaches one end of a line. On a failure it returns the reason of the line (wire.LineWaitingForDevice when the device is
// not there or not this lab's, wire.LinePortDown when the port is not the device's or the keeper refused the attach).
func (s *Service) attachPort(ctx context.Context, e endRef) (*portAttach, string, error) {
	spec, err := portkeeper.Parse(e.End.Ports)
	if err != nil {
		return nil, wire.LinePortDown, err
	}
	if e.Serial && !spec.HasSerial(e.End.Port) || !e.Serial && !spec.HasConsole(e.End.Port) {
		return nil, wire.LinePortDown, errors.New("the port is not in the port list with that kind")
	}
	dev, err := ResolveLabDevice(ctx, s.rt, Target{Namespace: e.Namespace, Lab: e.Lab, Device: e.End.Device})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrAmbiguous) {
			return nil, wire.LineWaitingForDevice, err
		}
		return nil, wire.LinePortDown, err
	}
	// The same names may belong to a lab that was deleted and made again: the pod must be this lab's.
	if e.LabUID == "" || dev.LabUID != e.LabUID {
		return nil, wire.LineWaitingForDevice, errors.New("the device is not of this lab")
	}
	// The port list the node-agent sent must be the one the operator stamped: a port it names is then the device's.
	if dev.Ports == "" || dev.Ports != portkeeper.Hash(spec) {
		return nil, wire.LinePortDown, errors.New("the port list is not the device's")
	}
	cspec, err := s.rt.Spec(ctx, dev.ContainerID)
	if err != nil || cspec.Process == nil {
		return nil, wire.LinePortDown, errors.Join(err, errors.New("no container spec"))
	}
	// Root in the device (the keeper's sockets are root only) with the container's own capability sets: port-attach needs no capability.
	proc, err := ProcessFor(cspec.Process, ExecArgs{Args: []string{names.ProbeBinary, portkeeper.CommandAttach, e.End.Port}, User: &ExecUser{UID: 0, GID: 0}})
	if err != nil {
		return nil, wire.LinePortDown, err
	}
	stdin, stdinW := newPipe(s.lim.WriteTimeout)
	outR, outW := io.Pipe()
	ex, err := s.rt.Start(ctx, dev.ContainerID, proc, IO{Stdin: stdin, Stdout: outW, Stderr: &limitedBuffer{max: 0}})
	if err != nil {
		stdinW.Abort()
		return nil, wire.LinePortDown, err
	}
	p := &portAttach{svc: s, ex: ex, stdin: stdinW, out: outR, buf: make([]byte, lineChunk)}
	exited := make(chan ExitStatus, 1)
	go func() {
		st, ok := <-ex.Wait()
		if ok {
			exited <- st
		}
		close(exited)
		// The runtime never closes the writer it copies into: the end of the exec is the end of its output.
		_ = outW.CloseWithError(io.EOF)
	}()
	first := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		_, err := io.ReadFull(outR, b)
		if err == nil && b[0] != portkeeper.AttachOK {
			err = errAttachRefused
		}
		first <- err
	}()
	timer := time.NewTimer(attachWait)
	defer timer.Stop()
	select {
	case err := <-first:
		if err == nil {
			return p, "", nil
		}
		// The exec is removed first; its exit code (when the runtime still reports it) says why the attach ended.
		p.close("")
		select {
		case st, ok := <-exited:
			if ok {
				return nil, wire.LinePortDown, attachExit(st)
			}
		case <-ctx.Done():
		case <-time.After(s.lim.Cleanup):
		}
		return nil, wire.LinePortDown, errAttachRefused
	case <-timer.C:
		p.close("")
		return nil, wire.LinePortDown, errors.New("the attach did not answer in time")
	case <-ctx.Done():
		p.close("")
		return nil, wire.LinePortDown, ctx.Err()
	}
}

// attachExit says why an attach ended before it was accepted (the exit codes of probe port-attach).
func attachExit(st ExitStatus) error {
	switch st.Code {
	case portkeeper.ExitBusy:
		return errors.New("the line has a client already")
	case portkeeper.ExitNoKeeper:
		return errors.New("no port keeper serves the port")
	case portkeeper.ExitPortFailed:
		return errors.New("the keeper could not make the port")
	case portkeeper.ExitUsage:
		return errors.New("the port name was refused")
	}
	return errAttachRefused
}

// relay moves bytes between the console side a and the serial side b until either side ends or ctx ends. It returns 0 when a ended
// first, 1 when b did, -1 when ctx ended; both sides are closed and both copies have stopped when it returns. reason names the close
// reason told to a stream peer from the side that ended.
func (s *Service) relay(ctx context.Context, a, b side, reason func(int) string) int {
	ended := make(chan int, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(src, dst side, srcIdx int, direction string) {
		defer wg.Done()
		lim := rate.NewLimiter(rate.Limit(names.ConsoleLineMaxBytesPerSecond), lineBurst)
		bytes := s.m.bytes.WithLabelValues("line", direction)
		for {
			p, err := src.read()
			if err != nil {
				ended <- srcIdx
				return
			}
			// at most lineBurst at a time: WaitN refuses more than the burst, which would end the line
			for len(p) > 0 {
				chunk := p[:min(len(p), lineBurst)]
				if err := lim.WaitN(ctx, len(chunk)); err != nil {
					ended <- -1
					return
				}
				if err := dst.write(chunk); err != nil {
					ended <- 1 - srcIdx
					return
				}
				bytes.Add(float64(len(chunk)))
				p = p[len(chunk):]
			}
		}
	}
	go pump(a, b, 0, "to-serial")
	go pump(b, a, 1, "to-console")
	var first int
	select {
	case first = <-ended:
	case <-ctx.Done():
		first = -1
	}
	r := wire.ReasonShutdown
	if first >= 0 && reason != nil {
		r = reason(first)
	}
	a.close(r)
	b.close(r)
	wg.Wait()
	return first
}
