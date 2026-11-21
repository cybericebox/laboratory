package deviceaccess

import (
	"context"
	"errors"
	"testing"

	"github.com/cybericebox/laboratory/internal/names"
)

func target() Target { return Target{Namespace: testNS, Lab: testLab, Device: testDev} }

func TestResolveFindsTheDeviceContainer(t *testing.T) {
	dev, err := Resolve(context.Background(), labRuntime(), target())
	if err != nil {
		t.Fatal(err)
	}
	if dev.ContainerID != cont1 || dev.PodUID != podUID {
		t.Fatalf("got %+v", dev)
	}
}

func TestResolveRefusals(t *testing.T) {
	type tc struct {
		name   string
		mutate func(rt *fakeRuntime)
		target func(Target) Target
		want   error
	}
	cases := []tc{
		{name: "wrong namespace", target: func(x Target) Target { x.Namespace = "lab-group-b"; return x }, want: ErrNotFound},
		{name: "wrong lab", target: func(x Target) Target { x.Lab = "other"; return x }, want: ErrNotFound},
		{name: "unknown device", target: func(x Target) Target { x.Device = "nope"; return x }, want: ErrNotFound},
		{name: "empty target", target: func(Target) Target { return Target{} }, want: ErrNotFound},
		{name: "console not enabled", want: ErrDisabled, mutate: func(rt *fakeRuntime) { delete(rt.containers[0].Labels, names.LabelTerminal) }},
		{name: "console label not true", want: ErrDisabled, mutate: func(rt *fakeRuntime) { rt.containers[0].Labels[names.LabelTerminal] = "yes" }},
		{name: "container not running", want: ErrNotFound, mutate: func(rt *fakeRuntime) { rt.containers[1].running = false }},
		{name: "VPN pod", want: ErrNotFound, mutate: func(rt *fakeRuntime) {
			// the operator's own components carry a component label and no device uid, and are named like a device here to be sure
			rt.containers[0].Labels[names.LabelComponent] = names.ComponentVPN
			delete(rt.containers[0].Labels, names.LabelDeviceUID)
		}},
		{name: "gateway pod even with both uids", want: ErrNotFound, mutate: func(rt *fakeRuntime) {
			rt.containers[0].Labels[names.LabelComponent] = names.ComponentGateway
		}},
		{name: "system pod without platform labels", want: ErrNotFound, mutate: func(rt *fakeRuntime) {
			rt.containers[0].Labels = map[string]string{labelKind: kindSandbox, labelPodNamespace: testNS, labelPodUID: podUID, names.LabelLab: testLab, names.LabelDevice: testDev}
		}},
		{name: "missing lab uid", want: ErrNotFound, mutate: func(rt *fakeRuntime) { delete(rt.containers[0].Labels, names.LabelLabUID) }},
		{name: "missing device uid", want: ErrNotFound, mutate: func(rt *fakeRuntime) { delete(rt.containers[0].Labels, names.LabelDeviceUID) }},
		{name: "labels only on the app container (a hostile image)", want: ErrNotFound, mutate: func(rt *fakeRuntime) {
			// the sandbox is a plain system pod; the image of the app container carries the platform labels
			rt.containers[0].Labels = map[string]string{labelKind: kindSandbox, labelPodNamespace: testNS, labelPodUID: podUID}
			for k, v := range sandboxLabels(podUID) {
				if rt.containers[1].Labels[k] == "" {
					rt.containers[1].Labels[k] = v
				}
			}
		}},
		{name: "only a helper container of the pod", want: ErrNotFound, mutate: func(rt *fakeRuntime) {
			rt.containers[1].Labels[labelContainer] = "cib-probe"
		}},
		{name: "container of another pod", want: ErrNotFound, mutate: func(rt *fakeRuntime) { rt.containers[1].Labels[labelPodUID] = "other" }},
		{name: "two running containers", want: ErrAmbiguous, mutate: func(rt *fakeRuntime) {
			rt.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: "c2", Labels: appLabels(podUID, testDev)}, running: true, spec: deviceSpec()})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := labRuntime()
			if c.mutate != nil {
				c.mutate(rt)
			}
			tg := target()
			if c.target != nil {
				tg = c.target(tg)
			}
			_, err := Resolve(context.Background(), rt, tg)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// A restarted container leaves a stopped container and a stopped sandbox of the same name behind; only the running one is the device.
func TestResolveIgnoresOldPods(t *testing.T) {
	rt := labRuntime()
	rt.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: "old-sb", Labels: sandboxLabels("old-uid")}})
	rt.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: "old-c", Labels: appLabels("old-uid", testDev)}, running: false})
	dev, err := Resolve(context.Background(), rt, target())
	if err != nil || dev.ContainerID != cont1 {
		t.Fatalf("got %+v, %v", dev, err)
	}
}
