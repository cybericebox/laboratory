package deviceaccess

import (
	"context"
	"errors"
	"fmt"

	"github.com/cybericebox/laboratory/internal/names"
)

// The labels the container runtime puts on the containers of a pod (the kubelet sets them, the CRI plugin passes them on).
const (
	labelPodNamespace = "io.kubernetes.pod.namespace"
	labelPodUID       = "io.kubernetes.pod.uid"
	labelContainer    = "io.kubernetes.container.name"
	labelKind         = "io.cri-containerd.kind"
	kindSandbox       = "sandbox"
	kindContainer     = "container"
)

// Target names a device. It is all a caller says about the container it wants.
type Target struct {
	Namespace, Lab, Device string
}

// Refusals of Resolve. The service tells a caller only the short code, not which one of the checks failed in detail.
var (
	// ErrNotFound: no lab device matches, or the pod behind the name is not a lab device at all (the VPN, the gateway, a system pod).
	ErrNotFound = errors.New("not a running lab device")
	// ErrDisabled: the device exists but its lab did not ask for a console.
	ErrDisabled = errors.New("the device has no console")
	// ErrAmbiguous: more than one running container answers to the name. The service refuses rather than guess.
	ErrAmbiguous = errors.New("more than one container answers to the device")
)

// Device is a lab device container.
type Device struct {
	ContainerID string
	PodUID      string
}

// Resolve finds the container of a lab device, and only that.
//
// How "this container is a lab device" is known, without the Kubernetes API: the pod labels are on the SANDBOX container of the
// pod in the runtime (the kubelet hands them to the CRI plugin as the labels of the sandbox, not of the app containers). The
// operator stamps the UIDs of the Lab and of the Device on the pod, and nothing else makes a pod with them: the platform's label
// prefix is stripped from every user label, and the VPN, the gateway and the system pods carry neither. So a sandbox with both UIDs,
// the right namespace, lab and device is a lab device. Labels are never read from the app container: the CRI copies the labels of
// the image onto it, and the image of a device is written by a participant.
//
// The device container is then the running container of the same pod (the same pod UID) whose name is the device name; the other
// containers of the pod (init containers, a helper) never match.
func Resolve(ctx context.Context, rt Runtime, t Target) (Device, error) {
	return resolve(ctx, rt, t, true)
}

// ResolveLabDevice is Resolve for a caller that needs no console on the device: the checker socket runs the probe in any lab device. It is
// the same white list (a sandbox with both UIDs of the operator and no component label, then the running container of that pod named
// like the device) without the console label.
func ResolveLabDevice(ctx context.Context, rt Runtime, t Target) (Device, error) {
	return resolve(ctx, rt, t, false)
}

func resolve(ctx context.Context, rt Runtime, t Target, needConsole bool) (Device, error) {
	if t.Namespace == "" || t.Lab == "" || t.Device == "" {
		return Device{}, ErrNotFound
	}
	sandboxes, err := rt.ListByLabels(ctx, map[string]string{
		labelKind:         kindSandbox,
		labelPodNamespace: t.Namespace,
		names.LabelLab:    t.Lab,
		names.LabelDevice: t.Device,
	})
	if err != nil {
		return Device{}, err
	}
	var found []Device
	disabled := false
	for _, sb := range sandboxes {
		l := sb.Labels
		uid := l[labelPodUID]
		// Operator-made device pods carry both UIDs and are never one of the operator's own components.
		if uid == "" || l[names.LabelLabUID] == "" || l[names.LabelDeviceUID] == "" || l[names.LabelComponent] != "" {
			continue
		}
		// The sandbox was selected by namespace, lab and device already; check them again on what came back, so a runtime or a fake
		// that matched loosely cannot widen the white list.
		if l[labelPodNamespace] != t.Namespace || l[names.LabelLab] != t.Lab || l[names.LabelDevice] != t.Device {
			continue
		}
		if needConsole && l[names.LabelTerminal] != "true" {
			disabled = true
			continue
		}
		cs, err := rt.ListByLabels(ctx, map[string]string{
			labelKind:         kindContainer,
			labelPodUID:       uid,
			labelPodNamespace: t.Namespace,
			labelContainer:    t.Device,
		})
		if err != nil {
			return Device{}, err
		}
		for _, c := range cs {
			if c.Labels[labelPodUID] != uid || c.Labels[labelContainer] != t.Device || c.Labels[labelKind] != kindContainer {
				continue
			}
			up, err := rt.Running(ctx, c.ID)
			if err != nil {
				return Device{}, err
			}
			if up {
				found = append(found, Device{ContainerID: c.ID, PodUID: uid})
			}
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return Device{}, fmt.Errorf("%w: %d", ErrAmbiguous, len(found))
	case disabled:
		return Device{}, ErrDisabled
	}
	return Device{}, ErrNotFound
}
