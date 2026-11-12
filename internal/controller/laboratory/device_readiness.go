package laboratory

import (
	"path/filepath"
	"strconv"
	"strings"

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

// readinessCommandFor is the argument list of the probe for the device, or nil when the device has nothing to wait for. It adds, for a device that runs a bootstrap, the marker of its success: the device is Ready only when the
// bootstrap succeeded AND the condition of the device holds; with no condition of its own, Ready follows the bootstrap.
func readinessCommandFor(device *laboratoryv1alpha1.Device, bootstrapped bool) []string {
	var args []string
	var files []string
	if bootstrapped {
		files = append(files, names.BootstrapOKFile)
	}
	if device.Spec.WaitReadyFile {
		files = append(files, names.ReadyFile)
	}
	if len(files) > 0 {
		args = append(args, "-file", strings.Join(files, string(filepath.ListSeparator)))
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

func webPort(device *laboratoryv1alpha1.Device) *laboratoryv1alpha1.DeviceAccess {
	return laboratoryv1alpha1.HTTPAccess(device.Spec.Access)
}

// withReadiness adds the readiness probe to the device container (the first), with what it needs: the emptyDir, the init
// container that fills it and the read-only mount. A device with nothing to wait for, or a platform without the node image
// to copy the probe from, is left as it is.
func (r *DeviceReconciler) withReadiness(device *laboratoryv1alpha1.Device, spec *corev1.PodSpec, bootstrapped bool) {
	command := readinessCommandFor(device, bootstrapped)
	if command == nil || r.NetConfigImage == "" {
		return
	}
	r.ensureProbeInstall(device, spec)
	c := &spec.Containers[0]
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
