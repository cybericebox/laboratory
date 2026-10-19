package laboratory

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

// Readiness of a device pod. A lab used to report a device as available as soon as its container started, even while the
// application inside was still booting. Now a device has a readiness probe when it publishes a web port (the application
// must answer on it) or asks to wait for /run/ready (WaitReadyFile); a device with neither is Ready when it runs, as before.
//
// The probe is an exec probe, run by the kubelet inside the container, not an httpGet from the node: the device pod has
// no route from the node's side that the platform guarantees (the access port is wired by the node-agent, the cluster
// network policy admits only the proxy), and an httpGet counts only 200-399, so an application that answers 401 or 404 on
// "/" would never be Ready. The program is the static probe of package internal/probe: a device image may carry no shell.
// It is readiness only: no liveness or startup probe, so nothing here ever restarts a container.
const (
	// readinessPeriodSeconds: the kubelet asks every two seconds, and a device is Ready at most this long after its application is.
	readinessPeriodSeconds = 2
	// readinessTimeoutSeconds covers the slowest address the probe tries (it tries every local address in turn).
	readinessTimeoutSeconds = 5
	// readinessFailureThreshold only matters for a Ready device (a NotReady one is checked every period regardless): it
	// takes 30 seconds of failures to take a device that was Ready out again, so one slow answer does not blink the lab.
	readinessFailureThreshold = 15
)

// readinessCommand is the argument list of the probe for the device, or nil when the device has nothing to wait for.
func readinessCommand(device *laboratoryv1alpha1.Device) []string {
	var args []string
	if device.Spec.WaitReadyFile {
		args = append(args, "-file", names.ReadyFile)
	}
	if web := webPort(device); web != nil {
		scheme := probe.SchemeTCP
		switch web.Protocol {
		case "", "http":
			scheme = probe.SchemeHTTP
		case "https":
			scheme = probe.SchemeHTTPS
		}
		args = append(args, "-port", strconv.Itoa(int(web.Port)), "-scheme", scheme)
	}
	if len(args) == 0 {
		return nil
	}
	return append([]string{names.ProbeBinary, "ready"}, args...)
}

func webPort(device *laboratoryv1alpha1.Device) *laboratoryv1alpha1.WebExposure {
	if device.Spec.Exposure == nil || device.Spec.Exposure.Web == nil || device.Spec.Exposure.Web.Port <= 0 {
		return nil
	}
	return device.Spec.Exposure.Web
}

// withReadiness adds the readiness probe to the device container (the first), with what it needs: the emptyDir, the init
// container that fills it and the read-only mount. A device with nothing to wait for, or a platform without the node image
// to copy the probe from, is left as it is.
func (r *DeviceReconciler) withReadiness(device *laboratoryv1alpha1.Device, spec *corev1.PodSpec) {
	command := readinessCommand(device)
	if command == nil || r.NetConfigImage == "" {
		return
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: names.ProbeVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: ptrQuantity(resource.MustParse("16Mi")),
		}},
	})
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:            names.ProbeInitContainer,
		Image:           r.deviceImage(device, r.NetConfigImage),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{names.ProbeImageBinary, "install", names.ProbeDir},
		VolumeMounts:    []corev1.VolumeMount{{Name: names.ProbeVolume, MountPath: names.ProbeDir}},
		// Requests equal limits, like every container of a device pod, so that the pod stays Guaranteed.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		},
		// It only copies a file into its own emptyDir: no capability, no escalation, no writable root file system.
		SecurityContext: &corev1.SecurityContext{
			Capabilities:             capsOf(),
			AllowPrivilegeEscalation: ptrBool(false),
			ReadOnlyRootFilesystem:   ptrBool(true),
		},
	})
	c := &spec.Containers[0]
	// Read-only: the participant is root in the device and must not be able to swap the probe for one that always says yes.
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: names.ProbeVolume, MountPath: names.ProbeDir, ReadOnly: true})
	c.ReadinessProbe = &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}},
		InitialDelaySeconds: 0,
		PeriodSeconds:       readinessPeriodSeconds,
		TimeoutSeconds:      readinessTimeoutSeconds,
		SuccessThreshold:    1,
		FailureThreshold:    readinessFailureThreshold,
	}
}

func ptrQuantity(q resource.Quantity) *resource.Quantity { return &q }
