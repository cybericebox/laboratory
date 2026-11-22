package deviceaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The lines of the node, as the node-agent sets them over cable.sock. The set is declarative: the node-agent puts the whole set every
// time, a line that is not in it is stopped, a line whose spec is the same keeps running (a node-agent that restarts finds its lines as
// they were), a changed line is stopped and started again.

// maxLinesBody bounds the body of a PUT of the lines (500 lines, each with two port lists of at most 64 KiB, would pass it only with
// absurd console commands).
const maxLinesBody = 32 << 20

var (
	serialPortRE  = regexp.MustCompile(`^serial(0|[1-9][0-9]?)$`)
	consolePortRE = regexp.MustCompile(`^(console|aux)(0|[1-9][0-9]?)$`)
)

// lineSet holds the lines.
type lineSet struct {
	svc   *Service
	mu    sync.Mutex
	lines map[string]*line
}

// line is one line of the node.
type line struct {
	set  *lineSet
	spec wire.LineSpec
	key  string // the JSON of the spec: a changed spec is another line

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	st      wire.LineState
	session *cableSession
}

// cableSession is the running cable stream of a serial-role line; a newer authenticated stream for the same line replaces it.
type cableSession struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func lineID(namespace, name string) string { return namespace + "/" + name }

func (l *line) setState(f func(*wire.LineState)) {
	l.mu.Lock()
	f(&l.st)
	l.mu.Unlock()
}

func (l *line) state() wire.LineState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

func (l *line) beginSession() *cableSession {
	ctx, cancel := context.WithCancel(l.ctx)
	s := &cableSession{ctx: ctx, cancel: cancel}
	l.mu.Lock()
	old := l.session
	l.session = s
	l.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	return s
}

func (l *line) endSession(s *cableSession) {
	s.cancel()
	l.mu.Lock()
	if l.session == s {
		l.session = nil
	}
	l.mu.Unlock()
}

// validateLine checks a line the node-agent sent before the service acts on it.
func validateLine(l wire.LineSpec) error {
	for _, f := range []string{l.Namespace, l.Name, l.UID, l.Lab, l.LabUID, l.Serial.Device, l.Console.Device} {
		if f == "" || len(f) > maxName {
			return errors.New("a name of the line is empty or too long")
		}
	}
	if !serialPortRE.MatchString(l.Serial.Port) || !consolePortRE.MatchString(l.Console.Port) {
		return errors.New("the ends of the line are not a serial port and a console port")
	}
	if l.Serial.Device == l.Console.Device {
		return errors.New("the two ends of a line are on two devices")
	}
	if len(l.Serial.Ports) == 0 || len(l.Console.Ports) == 0 || len(l.Serial.Ports) > 64<<10 || len(l.Console.Ports) > 64<<10 {
		return errors.New("a port list is missing or too large")
	}
	switch l.Role {
	case wire.RoleOwner:
		if l.Peer != "" {
			if _, _, err := net.SplitHostPort(l.Peer); err != nil {
				return errors.New("the peer is not host:port")
			}
		}
	case wire.RoleSerial:
		if l.Peer != "" {
			return errors.New("a serial-end line has no peer")
		}
	default:
		return errors.New("unknown role")
	}
	return nil
}

// SyncLines makes the lines of the node the given set and returns their state.
func (s *Service) SyncLines(req wire.LinesRequest) wire.LinesReply {
	set := s.lines
	desired := map[string]wire.LineSpec{}
	keys := map[string]string{}
	for _, l := range req.Lines {
		if err := validateLine(l); err != nil {
			s.log.Info("console line refused", "namespace", l.Namespace, "cable", l.Name, "reason", err.Error())
			continue
		}
		if len(desired) >= MaxLinesPerNode {
			s.log.Info("console line refused: too many lines on this node", "namespace", l.Namespace, "cable", l.Name, "limit", MaxLinesPerNode)
			continue
		}
		raw, _ := json.Marshal(l)
		id := lineID(l.Namespace, l.Name)
		desired[id], keys[id] = l, string(raw)
	}
	set.mu.Lock()
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	for id, l := range set.lines {
		if k, ok := keys[id]; !ok || k != l.key || stopping {
			l.cancel()
			delete(set.lines, id)
		}
	}
	if !stopping {
		for id, spec := range desired {
			if _, ok := set.lines[id]; ok {
				continue
			}
			set.lines[id] = set.start(spec, keys[id])
		}
	}
	set.mu.Unlock()
	return s.LineStates()
}

// LineStates is the state of every line of the node, sorted by cable.
func (s *Service) LineStates() wire.LinesReply {
	set := s.lines
	set.mu.Lock()
	out := make([]wire.LineState, 0, len(set.lines))
	up := map[string]int{}
	for _, l := range set.lines {
		st := l.state()
		out = append(out, st)
		state := "down"
		if st.Up {
			state = "up"
		}
		up[st.Role+"/"+state]++
	}
	set.mu.Unlock()
	for _, role := range []string{wire.RoleOwner, wire.RoleSerial} {
		for _, state := range []string{"up", "down"} {
			s.m.lines.WithLabelValues(role, state).Set(float64(up[role+"/"+state]))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return lineID(out[i].Namespace, out[i].Name) < lineID(out[j].Namespace, out[j].Name)
	})
	return wire.LinesReply{Relay: wire.RelayVersion, Lines: out}
}

func (set *lineSet) start(spec wire.LineSpec, key string) *line {
	ctx, cancel := context.WithCancel(context.Background())
	l := &line{set: set, spec: spec, key: key, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		st: wire.LineState{Namespace: spec.Namespace, Name: spec.Name, UID: spec.UID, Role: spec.Role, Reason: wire.LineWaitingForDevice}}
	s := set.svc
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		cancel()
		close(l.done)
		return l
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer close(l.done)
		if spec.Role == wire.RoleOwner {
			s.runOwner(l)
			return
		}
		// A serial-end line has no loop of its own: it waits for the owner's stream (serveCable).
		<-ctx.Done()
	}()
	return l
}

// accepting is the serial-end line that a cable stream asks for: the same cable (name and UID), the same lab and lab UID and the same two
// ends, given to this node with the serial-end role. nil otherwise.
func (set *lineSet) accepting(r wire.CableOpen) *line {
	set.mu.Lock()
	defer set.mu.Unlock()
	l := set.lines[lineID(r.Namespace, r.Name)]
	if l == nil {
		return nil
	}
	sp := l.spec
	if sp.Role != wire.RoleSerial || sp.UID != r.UID || sp.Lab != r.Lab || sp.LabUID != r.LabUID ||
		sp.Serial.Device != r.Serial.Device || sp.Serial.Port != r.Serial.Port || sp.Console.Device != r.Console.Device || sp.Console.Port != r.Console.Port {
		return nil
	}
	return l
}

// stopLines ends every line (Shutdown).
func (set *lineSet) stopAll() {
	set.mu.Lock()
	for id, l := range set.lines {
		l.cancel()
		delete(set.lines, id)
	}
	set.mu.Unlock()
}

// runOwner is the loop of a line whose console end runs on this node: attach the console end, attach the serial end or dial its node,
// relay, and when the line goes down, start again after the backoff.
func (s *Service) runOwner(l *line) {
	spec := l.spec
	ctx := l.ctx
	backoff := lineRetryMin
	wait := func() bool {
		t := time.NewTimer(backoff)
		defer t.Stop()
		backoff = min(2*backoff, lineRetryMax)
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	down := func(reason string, err error) {
		l.setState(func(st *wire.LineState) {
			if st.Up || st.Reason != reason {
				msg := ""
				if err != nil {
					msg = err.Error()
				}
				s.log.Info("console line down", "role", wire.RoleOwner, "namespace", spec.Namespace, "cable", spec.Name, "reason", reason, "detail", msg)
			}
			st.ConsoleAttached, st.SerialAttached, st.Up, st.Reason = false, false, false, reason
		})
	}
	for ctx.Err() == nil {
		console, reason, err := s.attachPort(ctx, endRef{Namespace: spec.Namespace, Lab: spec.Lab, LabUID: spec.LabUID, End: spec.Console})
		if err != nil {
			down(reason, err)
			if !wait() {
				return
			}
			continue
		}
		l.setState(func(st *wire.LineState) { st.ConsoleAttached = true })
		var serial side
		if spec.Peer == "" {
			var p *portAttach
			p, reason, err = s.attachPort(ctx, endRef{Namespace: spec.Namespace, Lab: spec.Lab, LabUID: spec.LabUID, End: spec.Serial, Serial: true})
			if p != nil {
				serial = p
			}
		} else {
			serial, reason, err = s.dialPeer(ctx, spec)
		}
		if err != nil {
			console.close("")
			down(reason, err)
			if !wait() {
				return
			}
			continue
		}
		l.setState(func(st *wire.LineState) {
			st.ConsoleAttached, st.SerialAttached, st.Up, st.Reason = true, true, true, ""
		})
		s.log.Info("console line up", "role", wire.RoleOwner, "namespace", spec.Namespace, "cable", spec.Name, "crossNode", spec.Peer != "")
		began := time.Now()
		first := s.relay(ctx, console, serial, func(int) string { return wire.ReasonExit })
		reason = wire.LinePortDown
		if first == 1 && spec.Peer != "" {
			reason = wire.LinePeerUnreachable
		}
		down(reason, nil)
		if time.Since(began) >= lineStable {
			backoff = lineRetryMin
		}
		if !wait() {
			return
		}
	}
}

// CableSockHandler serves cable.sock: the node-agent of this node sets the lines (PUT) and reads their state (GET). Mount it on a unix
// listener whose peer is checked (ListenCable).
func (s *Service) CableSockHandler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, r wire.LinesReply) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(r)
	}
	mux.HandleFunc("GET "+wire.LinesPath, func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, s.LineStates()) })
	mux.HandleFunc("PUT "+wire.LinesPath, func(w http.ResponseWriter, r *http.Request) {
		var req wire.LinesRequest
		raw, err := readAllLimited(r, maxLinesBody)
		if err == nil {
			dec := json.NewDecoder(bytes.NewReader(raw))
			// A field the service does not know is an error: there is no field for a program, a container or a user.
			dec.DisallowUnknownFields()
			err = dec.Decode(&req)
		}
		if err != nil {
			reply(w, http.StatusBadRequest, wire.LinesReply{Relay: wire.RelayVersion, Error: wire.CodeBadRequest})
			return
		}
		reply(w, http.StatusOK, s.SyncLines(req))
	})
	return mux
}

func readAllLimited(r *http.Request, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, limit))
	return buf.Bytes(), err
}
