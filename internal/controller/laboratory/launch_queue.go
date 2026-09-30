package laboratory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// This file holds the pure rules of launch pacing: which labs are queued, in
// which order they are admitted and what they need. The launcher applies them.

// autoClassPrefix marks a launch class derived from the lab itself.
const autoClassPrefix = "auto-"

// launchClassOf returns the launch class of a lab: the class the platform set in
// the spec, or a hash of the topology and images, so labs built from one
// template share a class even when the platform sends none.
func launchClassOf(lab *laboratoryv1alpha1.Lab) string {
	if c := strings.TrimSpace(lab.Spec.LaunchClass); c != "" {
		return c
	}
	lines := make([]string, 0, len(lab.Spec.Devices)+2)
	for _, d := range lab.Spec.Devices {
		lines = append(lines, fmt.Sprintf("device|%s|%s|%s|%s", d.Name, d.Type, d.Image, d.SecurityPreset))
	}
	lines = append(lines, fmt.Sprintf("vpn|%t", lab.Spec.VPN.Enabled), fmt.Sprintf("internet|%t", lab.Spec.Internet.Enabled))
	for _, c := range lab.Spec.Connections {
		var ends []string
		for _, e := range c.Endpoints {
			ends = append(ends, e.Device+"/"+e.Interface)
		}
		sort.Strings(ends)
		lines = append(lines, "connection|"+strings.Join(ends, ","))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return autoClassPrefix + hex.EncodeToString(sum[:])[:12]
}

// labAdmitted reports whether the launcher has let the lab start provisioning.
// A lab with a phase other than empty or Queued predates launch pacing (or was
// admitted and moved on), so it counts as admitted and is never re-queued.
func labAdmitted(lab *laboratoryv1alpha1.Lab) bool {
	if lab.Status.Launch != nil && lab.Status.Launch.AdmittedAt != nil {
		return true
	}
	return lab.Status.Phase != "" && lab.Status.Phase != laboratoryv1alpha1.PhaseQueued
}

// labQueued reports whether the lab waits in the queue.
func labQueued(lab *laboratoryv1alpha1.Lab) bool {
	return lab.DeletionTimestamp.IsZero() && !labAdmitted(lab)
}

// labInFlight reports whether an admitted lab still holds a launch slot: it was
// admitted by the launcher, is not finished (Ready, Failed, Error, Suspended)
// and its wave timeout has not expired.
func labInFlight(lab *laboratoryv1alpha1.Lab, now time.Time, waveTimeout time.Duration) bool {
	if !lab.DeletionTimestamp.IsZero() || lab.Status.Launch == nil || lab.Status.Launch.AdmittedAt == nil {
		return false
	}
	switch lab.Status.Phase {
	case laboratoryv1alpha1.PhaseReady, laboratoryv1alpha1.PhaseFailed,
		laboratoryv1alpha1.PhaseError, laboratoryv1alpha1.PhaseSuspended:
		return false
	}
	return now.Sub(lab.Status.Launch.AdmittedAt.Time) < waveTimeout
}

// orderQueue sorts queued labs into admission order: by launch class first, then
// by creation time. Classes are ordered by the oldest queued lab they hold, so
// the class that has waited longest is served first and all its labs are
// admitted before the next class starts.
func orderQueue(labs []*laboratoryv1alpha1.Lab) []*laboratoryv1alpha1.Lab {
	classOf := make(map[*laboratoryv1alpha1.Lab]string, len(labs))
	oldest := make(map[string]time.Time)
	for _, l := range labs {
		c := launchClassOf(l)
		classOf[l] = c
		if t, ok := oldest[c]; !ok || l.CreationTimestamp.Time.Before(t) {
			oldest[c] = l.CreationTimestamp.Time
		}
	}
	out := append([]*laboratoryv1alpha1.Lab(nil), labs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ca, cb := classOf[a], classOf[b]
		if ca != cb {
			if !oldest[ca].Equal(oldest[cb]) {
				return oldest[ca].Before(oldest[cb])
			}
			return ca < cb
		}
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return out
}

// classImages returns the sorted unique images of the container devices of labs.
func classImages(labs []*laboratoryv1alpha1.Lab) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range labs {
		for _, d := range l.Spec.Devices {
			if d.Type != laboratoryv1alpha1.DeviceTypeContainer || d.Image == "" || seen[d.Image] {
				continue
			}
			seen[d.Image] = true
			out = append(out, d.Image)
		}
	}
	sort.Strings(out)
	return out
}

// amount is a CPU and memory quantity: millicores and bytes.
type amount struct {
	cpu, mem int64
}

func (a amount) add(b amount) amount { return amount{a.cpu + b.cpu, a.mem + b.mem} }

func (a amount) sub(b amount) amount { return amount{a.cpu - b.cpu, a.mem - b.mem} }

func (a amount) floorZero() amount {
	if a.cpu < 0 {
		a.cpu = 0
	}
	if a.mem < 0 {
		a.mem = 0
	}
	return a
}

// labNeed is what the lab's device pods will request: the sum over container
// devices of their guaranteed resources (declared or default).
func labNeed(lab *laboratoryv1alpha1.Lab, defaults DeviceDefaults) amount {
	var sum amount
	for _, d := range lab.Spec.Devices {
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue
		}
		list := guaranteedResources(d.Resources, defaults)
		sum.cpu += list.Cpu().MilliValue()
		sum.mem += list.Memory().Value()
	}
	return sum
}

// podRequests is the amount the scheduler reserves for a pod: the larger of the
// sum of its containers and its biggest init container, plus the pod overhead.
func podRequests(pod *corev1.Pod) amount {
	var sum, initMax amount
	for i := range pod.Spec.Containers {
		r := pod.Spec.Containers[i].Resources.Requests
		sum.cpu += r.Cpu().MilliValue()
		sum.mem += r.Memory().Value()
	}
	for i := range pod.Spec.InitContainers {
		r := pod.Spec.InitContainers[i].Resources.Requests
		if v := r.Cpu().MilliValue(); v > initMax.cpu {
			initMax.cpu = v
		}
		if v := r.Memory().Value(); v > initMax.mem {
			initMax.mem = v
		}
	}
	if initMax.cpu > sum.cpu {
		sum.cpu = initMax.cpu
	}
	if initMax.mem > sum.mem {
		sum.mem = initMax.mem
	}
	sum.cpu += pod.Spec.Overhead.Cpu().MilliValue()
	sum.mem += pod.Spec.Overhead.Memory().Value()
	return sum
}
