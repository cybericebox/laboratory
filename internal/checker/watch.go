package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
)

// CacheOptions are the informers of a checker: the device pods of its own node only (a field selector the API server applies),
// and the Labs, stripped of everything the checker does not read. A Lab cannot be selected by node; the cost of watching them all
// is the one the node-agent already pays.
func CacheOptions(node string) (map[client.Object]cache.ByObject, error) {
	podSel, err := labels.Parse(names.LabelLab + "," + names.LabelDevice)
	if err != nil {
		return nil, err
	}
	return map[client.Object]cache.ByObject{
		&corev1.Pod{}: {
			Field:     fields.OneTermEqualSelector("spec.nodeName", node),
			Label:     podSel,
			Transform: trimPod,
		},
		&laboratoryv1alpha1.Lab{}: {Transform: trimLab},
	}, nil
}

// trimPod keeps the few fields of a pod the checker reads.
func trimPod(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	out := &corev1.Pod{}
	out.Name, out.Namespace, out.UID, out.Labels = pod.Name, pod.Namespace, pod.UID, pod.Labels
	out.DeletionTimestamp, out.ResourceVersion = pod.DeletionTimestamp, pod.ResourceVersion
	out.Spec.NodeName, out.Spec.RuntimeClassName = pod.Spec.NodeName, pod.Spec.RuntimeClassName
	out.Status.Phase = pod.Status.Phase
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			out.Status.Conditions = []corev1.PodCondition{{Type: c.Type, Status: c.Status}}
		}
	}
	return out, nil
}

// trimLab keeps what the checker reads of a Lab; a Lab without checks is kept as a stub.
func trimLab(obj any) (any, error) {
	lab, ok := obj.(*laboratoryv1alpha1.Lab)
	if !ok {
		return obj, nil
	}
	out := &laboratoryv1alpha1.Lab{}
	out.Name, out.Namespace, out.UID, out.ResourceVersion = lab.Name, lab.Namespace, lab.UID, lab.ResourceVersion
	out.DeletionTimestamp = lab.DeletionTimestamp
	if lab.Spec.Checks == nil || len(lab.Spec.Checks.Items) == 0 {
		return out, nil
	}
	for _, k := range []string{names.AnnotationRunChecks, names.AnnotationLabVariableAdmission, names.AnnotationLifecycleRetirement} {
		if v, ok := lab.Annotations[k]; ok {
			if out.Annotations == nil {
				out.Annotations = map[string]string{}
			}
			out.Annotations[k] = v
		}
	}
	out.Spec.Checks = lab.Spec.Checks
	for _, d := range lab.Spec.Devices {
		out.Spec.Devices = append(out.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{Name: d.Name, Bootstrap: d.Bootstrap})
	}
	out.Status.Checks, out.Status.ChecksRunAck = lab.Status.Checks, lab.Status.ChecksRunAck
	for _, d := range lab.Status.Devices {
		out.Status.Devices = append(out.Status.Devices, laboratoryv1alpha1.DeviceRef{Name: d.Name, Bootstrap: d.Bootstrap})
	}
	return out, nil
}

// LabInfoOf reads a (trimmed) Lab into what the engine needs.
func LabInfoOf(lab *laboratoryv1alpha1.Lab) LabInfo {
	info := LabInfo{Key: LabKey{Namespace: lab.Namespace, Name: lab.Name}, UID: string(lab.UID), Results: map[string]Published{},
		Ack: lab.Status.ChecksRunAck, BootstrapEnabled: map[string]bool{}, BootstrapPhase: map[string]laboratoryv1alpha1.BootstrapPhase{},
		Inactive: lab.DeletionTimestamp != nil || lab.Annotations[names.AnnotationLifecycleRetirement] != ""}
	if lab.Spec.Checks != nil {
		for _, it := range lab.Spec.Checks.Items {
			info.Refs = append(info.Refs, CheckRef{ID: it.ID, Device: it.Device, ManualRun: it.ManualRun})
		}
	}
	for _, r := range lab.Status.Checks {
		info.Results[r.ID] = PublishedFrom(r)
	}
	if req, ok := ParseRunRequest(lab.Annotations[names.AnnotationRunChecks]); ok {
		info.Request = req
	}
	for _, d := range lab.Spec.Devices {
		if d.Bootstrap != nil && d.Bootstrap.Enabled {
			info.BootstrapEnabled[d.Name] = true
		}
	}
	for _, d := range lab.Status.Devices {
		if d.Bootstrap != nil {
			info.BootstrapPhase[d.Name] = d.Bootstrap.Phase
		}
	}
	return info
}

// PodInfoOf reads a (trimmed) pod into what the engine needs.
func PodInfoOf(pod *corev1.Pod) (PodInfo, bool) {
	lab, device := pod.Labels[names.LabelLab], pod.Labels[names.LabelDevice]
	if lab == "" || device == "" {
		return PodInfo{}, false
	}
	ready := pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning
	if ready {
		ready = false
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
	}
	return PodInfo{Lab: LabKey{Namespace: pod.Namespace, Name: lab}, Device: device, Name: pod.Name, UID: string(pod.UID),
		Ready: ready, MicroVM: pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName != ""}, true
}

// ParseChecksSecret reads the checks document of a lab's Secret.
func ParseChecksSecret(s *corev1.Secret) ([]labchecks.Check, error) {
	raw, ok := s.Data[labchecks.ChecksKey]
	if !ok {
		return nil, fmt.Errorf("the Secret has no %s", labchecks.ChecksKey)
	}
	var doc labchecks.ChecksDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("the checks document does not parse: %w", err)
	}
	labchecks.NormalizeChecks(doc.Checks)
	return doc.Checks, nil
}

// Watcher feeds an Engine from the informers and fetches the checks Secret of a lab when it is needed.
//
// The Secret is read with one GET, not watched: a watch would bring every lab's checks to every node. A lab's Secret is read when
// the lab has a pod on this node and checks, and read again when the agent has rewritten it. The agent rewrites it under the claim
// annotation names.AnnotationLabVariableAdmission, which is on the Lab while the write is going and removed after; so "the claim
// came and went" is the signal to read again, and while the claim is on, nothing is read.
type Watcher struct {
	Engine *Engine
	// Reader is an uncached reader (the Secret is not in a cache).
	Reader client.Reader
	Log    logr.Logger
	// Retry is the first pause after a failed read of the Secret (doubled up to five minutes). Default 5 s.
	Retry time.Duration

	ctx  context.Context
	mu   sync.Mutex
	labs map[LabKey]*watched
}

type watched struct {
	uid       string
	hasChecks bool
	claim     bool
	stale     bool // the Secret must be read (again)
	loading   bool
	attempts  int
}

// Start registers the event handlers; ctx bounds the reads.
func (w *Watcher) Start(ctx context.Context, c cache.Cache) error {
	w.ctx, w.labs = ctx, map[LabKey]*watched{}
	if w.Retry == 0 {
		w.Retry = 5 * time.Second
	}
	pods, err := c.GetInformer(ctx, &corev1.Pod{})
	if err != nil {
		return fmt.Errorf("pod informer: %w", err)
	}
	if _, err := pods.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { w.onPod(o, false) },
		UpdateFunc: func(_, o any) { w.onPod(o, false) },
		DeleteFunc: func(o any) { w.onPod(o, true) },
	}); err != nil {
		return err
	}
	labs, err := c.GetInformer(ctx, &laboratoryv1alpha1.Lab{})
	if err != nil {
		return fmt.Errorf("lab informer: %w", err)
	}
	_, err = labs.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { w.onLab(o, false) },
		UpdateFunc: func(_, o any) { w.onLab(o, false) },
		DeleteFunc: func(o any) { w.onLab(o, true) },
	})
	return err
}

func unwrap[T any](o any) (T, bool) {
	if t, ok := o.(toolscache.DeletedFinalStateUnknown); ok {
		o = t.Obj
	}
	v, ok := o.(T)
	return v, ok
}

func (w *Watcher) onPod(o any, deleted bool) {
	pod, ok := unwrap[*corev1.Pod](o)
	if !ok {
		return
	}
	info, ok := PodInfoOf(pod)
	if !ok {
		return
	}
	if deleted {
		w.Engine.RemovePod(info.Lab, info.Device, info.UID)
		return
	}
	w.Engine.SetPod(info)
	w.mu.Lock()
	w.maybeLoad(info.Lab)
	w.mu.Unlock()
}

func (w *Watcher) onLab(o any, deleted bool) {
	lab, ok := unwrap[*laboratoryv1alpha1.Lab](o)
	if !ok {
		return
	}
	key := LabKey{Namespace: lab.Namespace, Name: lab.Name}
	if deleted {
		w.Engine.RemoveLab(key)
		w.mu.Lock()
		delete(w.labs, key)
		w.mu.Unlock()
		return
	}
	info := LabInfoOf(lab)
	w.Engine.SetLab(info)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(info.Refs) == 0 {
		delete(w.labs, key)
		return
	}
	st := w.labs[key]
	if st == nil || st.uid != info.UID {
		st = &watched{uid: info.UID, stale: true}
		w.labs[key] = st
	}
	st.hasChecks = len(info.Refs) > 0
	claim := lab.Annotations[names.AnnotationLabVariableAdmission] != ""
	if claim {
		st.claim, st.stale = true, true // a write is going; the Secret is read after it
	} else if st.claim {
		st.claim = false
	}
	w.maybeLoad(key)
}

// maybeLoad starts a read of the Secret when it is needed and possible. w.mu is held.
func (w *Watcher) maybeLoad(key LabKey) {
	st := w.labs[key]
	if st == nil || !st.hasChecks || !st.stale || st.claim || st.loading || !w.Engine.HasPods(key) {
		return
	}
	st.loading, st.stale = true, false
	uid := st.uid
	go w.load(key, uid)
}

func (w *Watcher) load(key LabKey, uid string) {
	var secret corev1.Secret
	ctx, cancel := context.WithTimeout(w.ctx, writeTimeout)
	defer cancel()
	err := w.Reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name + labchecks.ChecksSecretSuffix}, &secret)
	var checks []labchecks.Check
	if err == nil {
		checks, err = ParseChecksSecret(&secret)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.labs[key]
	if st == nil || st.uid != uid {
		return
	}
	st.loading = false
	if err != nil {
		st.attempts++
		pause := min(w.Retry<<min(st.attempts-1, 6), 5*time.Minute)
		w.Log.Info("cannot read the checks of a lab", "lab", key.String(), "error", err.Error(), "retryIn", pause.String())
		st.stale = true
		time.AfterFunc(pause, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.maybeLoad(key)
		})
		return
	}
	st.attempts = 0
	w.Engine.SetDefinitions(key, uid, checks)
	// A rewrite that came while this read was going marked the lab stale again.
	w.maybeLoad(key)
}
