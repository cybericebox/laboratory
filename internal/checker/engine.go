package checker

import (
	"context"
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
)

// LabKey names a Lab.
type LabKey struct{ Namespace, Name string }

func (k LabKey) String() string { return k.Namespace + "/" + k.Name }

// CheckRef is the part of a check that is on the Lab object (LabSpec.checks.items).
type CheckRef struct {
	ID        string
	Device    string
	ManualRun bool
}

// LabInfo is what the checker reads from a Lab object.
type LabInfo struct {
	Key  LabKey
	UID  string
	Refs []CheckRef
	// Results are the entries of status.checks; Ack is status.checksRunAck; Request is the run-checks annotation.
	Results map[string]Published
	Ack     int64
	Request RunRequest
	// BootstrapEnabled is spec.devices[].bootstrap.enabled; BootstrapPhase is status.devices[].bootstrap.phase, both by device name.
	BootstrapEnabled map[string]bool
	BootstrapPhase   map[string]laboratoryv1alpha1.BootstrapPhase
	// Inactive: the lab is retired or being deleted; nothing is run for it.
	Inactive bool
}

// PodInfo is a device pod on this node.
type PodInfo struct {
	Lab    LabKey
	Device string
	Name   string
	UID    string
	// Ready is the Ready condition of the pod. MicroVM: the pod runs in a micro-VM (it has a RuntimeClass), whose root file system
	// the node cannot see.
	Ready   bool
	MicroVM bool
}

// Backend gives access to a device. The returned function releases what Open took.
type Backend interface {
	Open(ctx context.Context, pod PodInfo) (Observer, func(), error)
}

// Writer publishes results. PublishCheck writes one entry of status.checks (and only that one); AckRun writes status.checksRunAck.
type Writer interface {
	PublishCheck(ctx context.Context, lab LabKey, result laboratoryv1alpha1.CheckResult) error
	AckRun(ctx context.Context, lab LabKey, token int64) error
}

// Config of an Engine.
type Config struct {
	Backend Backend
	Writer  Writer
	// MaxConcurrent is the most runs at once on this node.
	MaxConcurrent int
	// FirstRunWindow spreads the first run of the checks of a device that has just become ready (default 10 s).
	FirstRunWindow time.Duration
	Now            func() time.Time
	Log            logr.Logger
}

// Engine keeps the checks of the labs that have devices on this node, decides what runs when, runs it, and publishes changes.
// It holds no Kubernetes types except the status values; the informers feed it (see Watcher).
type Engine struct {
	cfg Config

	mu       sync.Mutex
	labs     map[LabKey]*labRuntime
	pods     map[LabKey]map[string]PodInfo
	inflight int
	wg       sync.WaitGroup
}

type entry struct {
	ref      CheckRef
	def      *labchecks.Check
	m        machine
	written  Published
	next     time.Time
	running  bool
	manual   bool
	runCount uint64
}

type batch struct {
	token int64
	left  map[string]bool
}

type labRuntime struct {
	info    LabInfo
	defs    map[string]*labchecks.Check
	loaded  bool
	entries map[string]*entry
	reqs    requests
	batches []*batch
	// ackWanted is the highest token whose work is done; ackWritten the highest written.
	ackWanted, ackWritten int64
	failures              int
	retryAt               time.Time
}

// NewEngine makes an Engine.
func NewEngine(cfg Config) *Engine {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.FirstRunWindow <= 0 {
		cfg.FirstRunWindow = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{cfg: cfg, labs: map[LabKey]*labRuntime{}, pods: map[LabKey]map[string]PodInfo{}}
}

// SetLab takes the current state of a Lab object (a new one, or a change).
func (e *Engine) SetLab(info LabInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(info.Refs) == 0 {
		delete(e.labs, info.Key) // a lab without checks is nothing to keep
		return
	}
	rt := e.labs[info.Key]
	if rt == nil || rt.info.UID != info.UID {
		rt = &labRuntime{defs: map[string]*labchecks.Check{}, entries: map[string]*entry{}}
		rt.reqs.startFrom(info.Ack)
		rt.ackWritten = info.Ack
		e.labs[info.Key] = rt
	}
	rt.info = info
	if info.Request.Token > 0 {
		rt.reqs.offer(info.Request)
	}
	rt.rebuild()
}

// RemoveLab forgets a Lab that is gone.
func (e *Engine) RemoveLab(key LabKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.labs, key)
}

// SetDefinitions takes the checks document of a lab (read from its Secret). uid guards against a document that arrives for a lab
// that was deleted and made again in the meantime.
func (e *Engine) SetDefinitions(key LabKey, uid string, checks []labchecks.Check) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.labs[key]
	if rt == nil || rt.info.UID != uid {
		return
	}
	rt.defs = make(map[string]*labchecks.Check, len(checks))
	for i := range checks {
		c := checks[i]
		rt.defs[c.ID] = &c
	}
	rt.loaded = true
	rt.rebuild()
}

// HasPods reports whether the lab has a device pod on this node.
func (e *Engine) HasPods(key LabKey) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pods[key]) > 0
}

// SetPod takes a device pod of this node (new, or changed).
func (e *Engine) SetPod(p PodInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pods[p.Lab] == nil {
		e.pods[p.Lab] = map[string]PodInfo{}
	}
	e.pods[p.Lab][p.Device] = p
}

// RemovePod forgets a pod that is gone.
func (e *Engine) RemovePod(lab LabKey, device, podUID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, ok := e.pods[lab][device]; ok && (podUID == "" || cur.UID == podUID) {
		delete(e.pods[lab], device)
		if len(e.pods[lab]) == 0 {
			delete(e.pods, lab)
		}
	}
}

// rebuild brings the entries in line with the references on the Lab and the definitions of its Secret. An entry keeps its state
// across a rebuild; a new one starts from what the status already holds, so a restart carries on where it was.
func (rt *labRuntime) rebuild() {
	for _, ref := range rt.info.Refs {
		def := rt.defs[ref.ID]
		if def == nil {
			continue
		}
		ent := rt.entries[ref.ID]
		if ent == nil {
			start := rt.info.Results[ref.ID]
			ent = &entry{m: machine{pub: start}, written: start}
			rt.entries[ref.ID] = ent
		}
		ent.ref, ent.def, ent.m.mode = ref, def, def.Mode
	}
}

// Job is one run, handed out by Tick.
type Job struct {
	Lab    LabKey
	Pod    PodInfo
	Check  labchecks.Check
	Manual bool
}

// deviceReady is the barrier: the pod is Ready and, when the device has a bootstrap, the bootstrap is done. A device that is not
// ready gets no run at all.
func (rt *labRuntime) deviceReady(pod PodInfo) bool {
	if !pod.Ready {
		return false
	}
	if rt.info.BootstrapEnabled[pod.Device] {
		return rt.info.BootstrapPhase[pod.Device] == laboratoryv1alpha1.BootstrapDone
	}
	return true
}

func (rt *labRuntime) dropFromBatches(id string) {
	for _, b := range rt.batches {
		delete(b.left, id)
	}
}

// startBatch turns a taken request into manual runs of the local checks it names.
func (rt *labRuntime) startBatch(req RunRequest, pods map[string]PodInfo) {
	want := map[string]bool{}
	for _, id := range req.IDs {
		want[id] = true
	}
	b := &batch{token: req.Token, left: map[string]bool{}}
	for id, ent := range rt.entries {
		if _, local := pods[ent.ref.Device]; !local || !ent.ref.ManualRun || (len(want) > 0 && !want[id]) {
			continue
		}
		b.left[id] = true
		ent.manual = true
	}
	if len(b.left) > 0 {
		rt.batches = append(rt.batches, b)
	}
}

// settleBatches moves the acknowledgement forward for every request whose local runs are all done (or dropped).
func (rt *labRuntime) settleBatches() {
	keep := rt.batches[:0]
	for _, b := range rt.batches {
		if len(b.left) == 0 {
			rt.ackWanted = max(rt.ackWanted, b.token)
			continue
		}
		keep = append(keep, b)
	}
	rt.batches = keep
}

// hashMs gives a stable number for a check: the spread of runs is a function of the lab, the check and the run count, so that
// the checks of many labs do not fire together and a test needs no random source.
func hashMs(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *Engine) firstDelay(uid, id string, schedule time.Duration) time.Duration {
	window := min(e.cfg.FirstRunWindow, schedule)
	if window <= 0 {
		return 0
	}
	return time.Duration(hashMs(uid, id, "first") % uint64(window.Milliseconds()+1) * uint64(time.Millisecond))
}

// nextAfter is the next due time after a run that started at now: the schedule, moved by up to 10 % either way by a number
// that changes with every run, so that the phases of many checks drift apart instead of staying together.
func nextAfter(now time.Time, schedule time.Duration, uid, id string, n uint64) time.Time {
	span := schedule.Milliseconds() / 10
	delta := int64(hashMs(uid, id, "next", strconv.FormatUint(n, 10))%uint64(2*span+1)) - span
	return now.Add(schedule + time.Duration(delta)*time.Millisecond)
}

// Tick decides what runs now: it takes new manual requests, applies the barrier and returns the runs that are due, earliest first,
// at most as many as there is room for. It does no I/O.
func (e *Engine) Tick(now time.Time) []Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	type cand struct {
		job Job
		ent *entry
		due time.Time
		rt  *labRuntime
	}
	var cands []cand
	keys := make([]LabKey, 0, len(e.labs))
	for k := range e.labs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, key := range keys {
		rt := e.labs[key]
		pods := e.pods[key]
		if rt.info.Inactive || len(pods) == 0 {
			continue
		}
		if rt.loaded {
			if req, ok := rt.reqs.take(); ok {
				rt.startBatch(req, pods)
			}
		}
		ids := make([]string, 0, len(rt.entries))
		for id := range rt.entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			ent := rt.entries[id]
			pod, local := pods[ent.ref.Device]
			if !local {
				continue
			}
			if !rt.deviceReady(pod) {
				ent.next, ent.manual = time.Time{}, false
				ent.m.reset()
				rt.dropFromBatches(id)
				continue
			}
			if ent.m.finished() {
				ent.manual = false
				rt.dropFromBatches(id)
				continue
			}
			if ent.running {
				continue
			}
			schedule := time.Duration(ent.def.ScheduleSeconds) * time.Second
			due := ent.manual
			dueAt := now
			if schedule > 0 {
				if ent.next.IsZero() {
					ent.next = now.Add(e.firstDelay(rt.info.UID, id, schedule))
				}
				if !ent.next.After(now) {
					due, dueAt = true, ent.next
				}
			}
			if ent.manual {
				dueAt = time.Time{} // a person is waiting: before every scheduled run
			}
			if due {
				cands = append(cands, cand{Job{Lab: key, Pod: pod, Check: *ent.def, Manual: ent.manual}, ent, dueAt, rt})
			}
		}
		rt.settleBatches()
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].due.Before(cands[j].due) })
	room := e.cfg.MaxConcurrent - e.inflight
	var jobs []Job
	for _, c := range cands {
		if room <= 0 {
			break
		}
		room--
		e.inflight++
		c.ent.running, c.ent.manual = true, false
		c.ent.runCount++
		if schedule := time.Duration(c.job.Check.ScheduleSeconds) * time.Second; schedule > 0 {
			c.ent.next = nextAfter(now, schedule, c.rt.info.UID, c.job.Check.ID, c.ent.runCount)
		}
		jobs = append(jobs, c.job)
	}
	return jobs
}

// Execute makes one run and folds the result in. It is safe to call from several goroutines.
func (e *Engine) Execute(ctx context.Context, job Job) {
	defer func() {
		e.mu.Lock()
		e.inflight--
		e.mu.Unlock()
	}()
	limit := time.Duration(job.Check.TimeLimitSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var run Run
	obs, release, err := e.cfg.Backend.Open(ctx, job.Pod)
	if err != nil {
		e.cfg.Log.V(1).Info("cannot open the device", "lab", job.Lab.String(), "check", job.Check.ID, "error", err.Error())
		run = Run{Outcome: Failed, Detail: reasonOf(ctx, err)}
	} else {
		run = RunCheck(ctx, &job.Check, obs)
		release()
	}
	e.finish(job, run)
}

func (e *Engine) finish(job Job, run Run) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.labs[job.Lab]
	if rt == nil {
		return
	}
	ent := rt.entries[job.Check.ID]
	if ent == nil {
		return
	}
	ent.running = false
	rt.dropFromBatches(job.Check.ID)
	defer rt.settleBatches()
	// The device may have left the barrier while the run was going (the pod restarted): that run counts for nothing.
	if pod, ok := e.pods[job.Lab][job.Pod.Device]; !ok || !rt.deviceReady(pod) || pod.UID != job.Pod.UID {
		ent.m.reset()
		return
	}
	ent.m.apply(e.cfg.Now(), run, job.Manual)
}

// Flush writes what changed: one entry per changed check, then the acknowledgements of the requests whose work is done and whose
// results are written. A failed write is retried with a growing pause; nothing else is lost meanwhile (the entry stays dirty).
func (e *Engine) Flush(ctx context.Context) {
	now := e.cfg.Now()
	type write struct {
		lab LabKey
		id  string
		pub Published
	}
	var writes []write
	e.mu.Lock()
	for key, rt := range e.labs {
		if now.Before(rt.retryAt) {
			continue
		}
		for id, ent := range rt.entries {
			if ent.m.pub != ent.written {
				writes = append(writes, write{key, id, ent.m.pub})
			}
		}
	}
	e.mu.Unlock()
	sort.Slice(writes, func(i, j int) bool {
		if writes[i].lab != writes[j].lab {
			return writes[i].lab.String() < writes[j].lab.String()
		}
		return writes[i].id < writes[j].id
	})
	failed := map[LabKey]bool{}
	wrote := map[LabKey]bool{}
	for _, w := range writes {
		if failed[w.lab] {
			continue
		}
		if err := e.cfg.Writer.PublishCheck(ctx, w.lab, w.pub.Result(w.id)); err != nil {
			e.cfg.Log.Info("cannot publish a check result", "lab", w.lab.String(), "check", w.id, "error", err.Error())
			failed[w.lab] = true
			continue
		}
		wrote[w.lab] = true
		e.mu.Lock()
		if rt := e.labs[w.lab]; rt != nil && rt.entries[w.id] != nil {
			rt.entries[w.id].written = w.pub
		}
		e.mu.Unlock()
	}
	// Acknowledgements: only for a lab whose results are all written (the token must never overtake a result).
	type ackJob struct {
		lab   LabKey
		token int64
	}
	var acks []ackJob
	e.mu.Lock()
	for key, rt := range e.labs {
		if failed[key] || now.Before(rt.retryAt) || rt.ackWanted <= rt.ackWritten {
			continue
		}
		clean := true
		for _, ent := range rt.entries {
			if ent.m.pub != ent.written {
				clean = false
			}
		}
		if clean {
			acks = append(acks, ackJob{key, rt.ackWanted})
		}
	}
	e.mu.Unlock()
	for _, a := range acks {
		if err := e.cfg.Writer.AckRun(ctx, a.lab, a.token); err != nil {
			e.cfg.Log.Info("cannot acknowledge a manual run", "lab", a.lab.String(), "token", a.token, "error", err.Error())
			failed[a.lab] = true
			continue
		}
		wrote[a.lab] = true
		e.mu.Lock()
		if rt := e.labs[a.lab]; rt != nil {
			rt.ackWritten = max(rt.ackWritten, a.token)
		}
		e.mu.Unlock()
	}
	e.mu.Lock()
	for key, rt := range e.labs {
		switch {
		case failed[key]:
			rt.failures++
			rt.retryAt = now.Add(min(time.Duration(1<<min(rt.failures, 6))*time.Second, time.Minute))
		case wrote[key]:
			rt.failures, rt.retryAt = 0, time.Time{}
		}
	}
	e.mu.Unlock()
}

// Run drives the engine until ctx ends: every second it asks for the runs that are due and starts them, and flushes changes.
func (e *Engine) Run(ctx context.Context, beat func()) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for _, job := range e.Tick(e.cfg.Now()) {
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				e.Execute(ctx, job)
			}()
		}
		e.Flush(ctx)
		if beat != nil {
			beat()
		}
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return
		case <-t.C:
		}
	}
}
