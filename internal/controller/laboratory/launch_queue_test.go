package laboratory

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
)

var testEpoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// queuedLab builds a lab with one container device, created sec seconds after the epoch.
func queuedLab(name, class, image string, sec int) *laboratoryv1alpha1.Lab {
	l := &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "team-" + name, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(testEpoch.Add(time.Duration(sec) * time.Second)),
		},
		Spec: laboratoryv1alpha1.LabSpec{
			LaunchClass: class,
			Devices:     []laboratoryv1alpha1.DeviceTemplate{{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: image}},
		},
	}
	return l
}

func names_(labs []*laboratoryv1alpha1.Lab) []string {
	var out []string
	for _, l := range labs {
		out = append(out, l.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLaunchClassExplicitWins(t *testing.T) {
	l := queuedLab("a", "  sqli-v2 ", "img", 0)
	if got := launchClassOf(l); got != "sqli-v2" {
		t.Fatalf("class = %q", got)
	}
}

// Labs built from one template share a derived class whatever their names and
// creation time; a different image or topology gives another class.
func TestLaunchClassDerivedFromTopology(t *testing.T) {
	a := queuedLab("a", "", "reg/web:1", 0)
	b := queuedLab("b", "", "reg/web:1", 99)
	if launchClassOf(a) != launchClassOf(b) {
		t.Fatal("identical topologies must share a class")
	}
	if got := launchClassOf(a); len(got) != len(autoClassPrefix)+12 || got[:len(autoClassPrefix)] != autoClassPrefix {
		t.Fatalf("derived class %q", got)
	}
	c := queuedLab("c", "", "reg/web:2", 0)
	if launchClassOf(a) == launchClassOf(c) {
		t.Fatal("another image must give another class")
	}
	d := queuedLab("d", "", "reg/web:1", 0)
	d.Spec.Internet.Enabled = true
	if launchClassOf(a) == launchClassOf(d) {
		t.Fatal("another topology must give another class")
	}
	// Device order in the spec does not matter.
	e := queuedLab("e", "", "x", 0)
	e.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "a", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "i1"},
		{Name: "b", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "i2"},
	}
	f := e.DeepCopy()
	f.Spec.Devices[0], f.Spec.Devices[1] = f.Spec.Devices[1], f.Spec.Devices[0]
	if launchClassOf(e) != launchClassOf(f) {
		t.Fatal("device order must not change the class")
	}
}

// One class is served completely before the next, classes in order of their
// oldest queued lab, labs inside a class by creation time.
func TestOrderQueueByClassThenCreation(t *testing.T) {
	labs := []*laboratoryv1alpha1.Lab{
		queuedLab("b1", "B", "i", 10),
		queuedLab("a2", "A", "i", 30),
		queuedLab("c1", "C", "i", 20),
		queuedLab("a1", "A", "i", 40),
		queuedLab("b2", "B", "i", 50),
		queuedLab("a0", "A", "i", 5),
	}
	got := names_(orderQueue(labs))
	want := []string{"a0", "a2", "a1", "b1", "b2", "c1"}
	if !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	// The input is not reordered.
	if labs[0].Name != "b1" {
		t.Fatal("orderQueue must not modify its input")
	}
}

// Equal creation times fall back to a stable order, so ticks agree with each other.
func TestOrderQueueTieBreak(t *testing.T) {
	labs := []*laboratoryv1alpha1.Lab{queuedLab("z", "A", "i", 1), queuedLab("m", "A", "i", 1), queuedLab("b", "B", "i", 1)}
	got := names_(orderQueue(labs))
	if !equalStrings(got, []string{"m", "z", "b"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestLabAdmittedLegacyAndQueued(t *testing.T) {
	l := queuedLab("a", "A", "i", 0)
	if labAdmitted(l) || !labQueued(l) {
		t.Fatal("a new lab is queued")
	}
	l.Status.Phase = laboratoryv1alpha1.PhaseQueued
	if labAdmitted(l) {
		t.Fatal("Queued is not admitted")
	}
	for _, ph := range []laboratoryv1alpha1.Phase{
		laboratoryv1alpha1.PhaseProvisioning, laboratoryv1alpha1.PhaseReady, laboratoryv1alpha1.PhaseFailed,
	} {
		l.Status.Phase = ph
		if !labAdmitted(l) {
			t.Fatalf("a lab in phase %s predates pacing and counts as admitted", ph)
		}
	}
	l.Status.Phase = ""
	at := metav1.NewTime(testEpoch)
	l.Status.Launch = &laboratoryv1alpha1.LabLaunchStatus{AdmittedAt: &at}
	if !labAdmitted(l) {
		t.Fatal("an admission time admits")
	}
	del := metav1.NewTime(testEpoch)
	l.Status.Launch = nil
	l.DeletionTimestamp = &del
	if labQueued(l) {
		t.Fatal("a lab being deleted is not queued")
	}
}

func TestLabInFlight(t *testing.T) {
	const timeout = 3 * time.Minute
	at := metav1.NewTime(testEpoch)
	mk := func(ph laboratoryv1alpha1.Phase) *laboratoryv1alpha1.Lab {
		l := queuedLab("a", "A", "i", 0)
		l.Status.Phase = ph
		l.Status.Launch = &laboratoryv1alpha1.LabLaunchStatus{AdmittedAt: &at}
		return l
	}
	if !labInFlight(mk(laboratoryv1alpha1.PhaseProvisioning), testEpoch.Add(time.Minute), timeout) {
		t.Fatal("provisioning inside the timeout is in flight")
	}
	if labInFlight(mk(laboratoryv1alpha1.PhaseProvisioning), testEpoch.Add(timeout), timeout) {
		t.Fatal("the slot is released when the wave timeout expires")
	}
	for _, ph := range []laboratoryv1alpha1.Phase{
		laboratoryv1alpha1.PhaseReady, laboratoryv1alpha1.PhaseFailed, laboratoryv1alpha1.PhaseError, laboratoryv1alpha1.PhaseSuspended,
	} {
		if labInFlight(mk(ph), testEpoch.Add(time.Second), timeout) {
			t.Fatalf("a %s lab does not hold a slot", ph)
		}
	}
	legacy := queuedLab("old", "A", "i", 0)
	legacy.Status.Phase = laboratoryv1alpha1.PhaseProvisioning
	if labInFlight(legacy, testEpoch, timeout) {
		t.Fatal("a lab admitted before pacing existed holds no slot")
	}
}

func TestClassImages(t *testing.T) {
	a := queuedLab("a", "A", "img-b", 0)
	a.Spec.Devices = append(a.Spec.Devices,
		laboratoryv1alpha1.DeviceTemplate{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch, Image: "ignored"},
		laboratoryv1alpha1.DeviceTemplate{Name: "db", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "img-a"},
		laboratoryv1alpha1.DeviceTemplate{Name: "noimg", Type: laboratoryv1alpha1.DeviceTypeContainer},
	)
	b := queuedLab("b", "A", "img-b", 0)
	got := classImages([]*laboratoryv1alpha1.Lab{a, b}, imagecache.Rewriter{})
	if !equalStrings(got, []string{"img-a", "img-b"}) {
		t.Fatalf("images = %v", got)
	}
}

func TestLabNeedSumsContainerDevices(t *testing.T) {
	l := queuedLab("a", "A", "i", 0)
	l.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "a", Type: laboratoryv1alpha1.DeviceTypeContainer}, // defaults
		{Name: "b", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: &laboratoryv1alpha1.DeviceResources{CPULimit: "1", MemoryRequest: "1Gi"}},
		{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
	}
	got := labNeed(l, DeviceDefaults{CPU: "250m", Memory: "256Mi"})
	want := amount{cpu: 250 + 1000, mem: (256 + 1024) << 20}
	if got != want {
		t.Fatalf("need = %+v, want %+v", got, want)
	}
	if got := labNeed(l, DeviceDefaults{}); got != (amount{cpu: 1000, mem: 1 << 30}) {
		t.Fatalf("need without defaults = %+v", got)
	}
}
