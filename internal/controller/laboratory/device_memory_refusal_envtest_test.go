package laboratory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// Owner decision C4: a device whose memory is above limits.device.maxMemory is refused, not clamped. The Device gets no workload and
// Ready=False with the reason DeviceMemoryAboveMaximum, a message naming the requested and the maximum amounts and one Warning event;
// a Lab with such a device fails with the same reason and none of its devices is created. A device at the maximum starts at its size.
func TestDeviceMemoryAboveMaximumIsRefused(t *testing.T) {
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir()}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const ns = "memory-maximum"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	defaults := DeviceDefaults{CPU: "100m", Memory: "256Mi", MaxCPU: "2", MaxMemory: "4Gi"}
	device := func(name, memory string) *lab.Device {
		d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "l-" + name, Namespace: ns}, Spec: lab.DeviceSpec{Name: name, Type: lab.DeviceTypeContainer, Image: "nginx:alpine", Code: "c" + name,
			Resources: &lab.DeviceResources{MemoryLimit: memory}}}
		if err := c.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	t.Run("device above the maximum", func(t *testing.T) {
		rec := record.NewFakeRecorder(10)
		r := &DeviceReconciler{Client: c, Scheme: scheme, Recorder: rec, Defaults: defaults}
		d := device("big", "8Gi")
		for range 2 { // a second pass changes nothing and records no second event
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
				t.Fatal(err)
			}
		}
		var dep appsv1.Deployment
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName(d)}, &dep); !apierrors.IsNotFound(err) {
			t.Fatalf("a refused device must get no workload: %v %+v", err, dep.Spec.Template.Spec.Containers)
		}
		var got lab.Device
		if err := c.Get(ctx, client.ObjectKeyFromObject(d), &got); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, labstatus.ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != limits.ReasonDeviceMemoryAboveMaximum || got.Status.Ready ||
			!strings.Contains(cond.Message, "8Gi") || !strings.Contains(cond.Message, "4Gi") {
			t.Fatalf("condition %+v ready %v", cond, got.Status.Ready)
		}
		if got.Status.Scheduling != nil {
			t.Errorf("a refused device is not queued: %+v", got.Status.Scheduling)
		}
		if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning "+limits.ReasonDeviceMemoryAboveMaximum) {
			t.Errorf("event: %q", ev)
		}
		select {
		case ev := <-rec.Events:
			t.Errorf("a second event: %q", ev)
		default:
		}
	})

	t.Run("device at the maximum", func(t *testing.T) {
		r := &DeviceReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10), Defaults: defaults}
		d := device("at", "4Gi")
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName(d)}, &dep); err != nil {
			t.Fatalf("a device at the maximum starts: %v", err)
		}
		if m := dep.Spec.Template.Spec.Containers[0].Resources.Limits.Memory(); m.Value() != 4<<30 {
			t.Fatalf("memory limit %v", m)
		}
		var got lab.Device
		if err := c.Get(ctx, client.ObjectKeyFromObject(d), &got); err != nil {
			t.Fatal(err)
		}
		if cond := meta.FindStatusCondition(got.Status.Conditions, labstatus.ConditionReady); cond != nil {
			t.Fatalf("no refusal condition: %+v", cond)
		}
	})

	// Review item 3: a device whose workload exists already (made before this rule, or before the maximum was lowered) is left alone:
	// the Deployment stays, the device is not refused, and it is warned once that it is above the current maximum.
	t.Run("device whose workload exists", func(t *testing.T) {
		rec := record.NewFakeRecorder(10)
		r := &DeviceReconciler{Client: c, Scheme: scheme, Recorder: rec, Defaults: defaults}
		d := device("running", "8Gi")
		labels, selector, _, spec := r.workloadTemplate(d)
		one := int32(1)
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: workloadName(d), Namespace: ns, Labels: labels},
			Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec}}}
		if err := c.Create(ctx, dep); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(dep), dep); err != nil || dep.DeletionTimestamp != nil {
			t.Fatalf("the running workload must stay: %v", err)
		}
		var got lab.Device
		if err := c.Get(ctx, client.ObjectKeyFromObject(d), &got); err != nil {
			t.Fatal(err)
		}
		if cond := meta.FindStatusCondition(got.Status.Conditions, labstatus.ConditionReady); cond != nil {
			t.Fatalf("a running device is not refused: %+v", cond)
		}
		warn := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryAboveMaximum)
		if warn == nil || warn.Status != metav1.ConditionTrue || warn.Reason != limits.ReasonDeviceMemoryAboveMaximum || !strings.Contains(warn.Message, "8Gi") {
			t.Fatalf("warning condition %+v", warn)
		}
		if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning "+limits.ReasonDeviceMemoryAboveMaximum) {
			t.Errorf("event: %q", ev)
		}
		select {
		case ev := <-rec.Events:
			t.Errorf("a second event: %q", ev)
		default:
		}
	})

	// Review item 2: a device that already ran (its pod ended, or the scheduler withdrew it) is recreated at its declared size and
	// warned, not refused: its scheduling record keeps meaning a pod.
	t.Run("device that already ran", func(t *testing.T) {
		r := &DeviceReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10), Defaults: defaults}
		d := device("ran", "8Gi")
		now := metav1.Now()
		d.Status.Scheduling = &lab.PodSchedule{State: lab.PodStarted, DispatchedAt: &now, StartedAt: &now}
		if err := c.Status().Update(ctx, d); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName(d)}, &dep); err != nil {
			t.Fatalf("a device that ran is recreated: %v", err)
		}
		if m := dep.Spec.Template.Spec.Containers[0].Resources.Limits.Memory(); m.Value() != 8<<30 {
			t.Fatalf("recreated at its declared size: %v", m)
		}
		var got lab.Device
		if err := c.Get(ctx, client.ObjectKeyFromObject(d), &got); err != nil {
			t.Fatal(err)
		}
		if meta.FindStatusCondition(got.Status.Conditions, labstatus.ConditionReady) != nil || meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryAboveMaximum) == nil {
			t.Fatalf("conditions %+v", got.Status.Conditions)
		}
	})

	// Review item 1: a running lab whose device is above a lowered maximum keeps running: no Failed, a warning condition and event.
	t.Run("running lab above a lowered maximum", func(t *testing.T) {
		rec := record.NewFakeRecorder(10)
		r := &LabReconciler{Client: c, Scheme: scheme, Recorder: rec, Defaults: defaults}
		l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: ns}, Spec: lab.LabSpec{Devices: []lab.DeviceTemplate{
			{Name: "big", Type: lab.DeviceTypeContainer, Image: "nginx:alpine", Resources: &lab.DeviceResources{MemoryLimit: "8Gi"}},
		}}}
		if err := c.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
		existing := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "running-big", Namespace: ns, Labels: map[string]string{names.LabelLab: l.Name}},
			Spec: lab.DeviceSpec{LabRef: l.Name, Name: "big", Type: lab.DeviceTypeContainer, Image: "nginx:alpine", Code: "crb", Resources: &lab.DeviceResources{MemoryLimit: "8Gi"}}}
		if err := c.Create(ctx, existing); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if refusal, _, err := r.validateGatewaysAndServers(ctx, l); err != nil || refusal != nil {
				t.Fatalf("a lab whose device exists is not refused: %v %v", refusal, err)
			}
		}
		var got lab.Lab
		if err := c.Get(ctx, client.ObjectKeyFromObject(l), &got); err != nil {
			t.Fatal(err)
		}
		warn := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryAboveMaximum)
		if got.Status.Phase == lab.PhaseFailed || warn == nil || warn.Status != metav1.ConditionTrue || warn.Reason != limits.ReasonDeviceMemoryAboveMaximum ||
			!strings.Contains(warn.Message, `"big"`) || !strings.Contains(warn.Message, "4Gi") {
			t.Fatalf("phase %s warning %+v", got.Status.Phase, warn)
		}
		if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning "+limits.ReasonDeviceMemoryAboveMaximum) {
			t.Errorf("event: %q", ev)
		}
		select {
		case ev := <-rec.Events:
			t.Errorf("a second event: %q", ev)
		default:
		}
	})

	t.Run("lab with a device above the maximum", func(t *testing.T) {
		rec := record.NewFakeRecorder(10)
		r := &LabReconciler{Client: c, Scheme: scheme, Recorder: rec, Defaults: defaults, LabBaseNetwork: "10.128.0.0/9"}
		l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "refused", Namespace: ns}, Spec: lab.LabSpec{Devices: []lab.DeviceTemplate{
			{Name: "ok", Type: lab.DeviceTypeContainer, Image: "nginx:alpine", Resources: &lab.DeviceResources{MemoryLimit: "4Gi"}},
			{Name: "big", Type: lab.DeviceTypeContainer, Image: "nginx:alpine", Resources: &lab.DeviceResources{MemoryLimit: "8Gi"}},
		}}}
		if err := c.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l)}); err != nil {
			t.Fatal(err)
		}
		var got lab.Lab
		if err := c.Get(ctx, client.ObjectKeyFromObject(l), &got); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, labstatus.ConditionReady)
		if got.Status.Phase != lab.PhaseFailed || cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != limits.ReasonDeviceMemoryAboveMaximum ||
			!strings.Contains(cond.Message, `"big"`) || !strings.Contains(cond.Message, "8Gi") || !strings.Contains(cond.Message, "4Gi") {
			t.Fatalf("phase %s condition %+v", got.Status.Phase, cond)
		}
		var devices lab.DeviceList
		if err := c.List(ctx, &devices, client.InNamespace(ns), client.MatchingLabels{"cybericebox.com/lab": l.Name}); err != nil {
			t.Fatal(err)
		}
		for _, d := range devices.Items {
			if d.Spec.LabRef == l.Name {
				t.Fatalf("a refused lab creates no devices: %s", d.Name)
			}
		}
		if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning "+limits.ReasonDeviceMemoryAboveMaximum) {
			t.Errorf("event: %q", ev)
		}
		// the same lab at the maximum gets past this check
		at := l.DeepCopy()
		at.Spec.Devices = at.Spec.Devices[:1]
		if refusal, _, err := r.validateGatewaysAndServers(ctx, at); err != nil || refusal != nil {
			t.Fatalf("a lab at the maximum: %v %v", refusal, err)
		}
	})
}
