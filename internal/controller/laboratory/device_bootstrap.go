package laboratory

import (
	"context"
	stderrors "errors"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/launcher"
	"github.com/cybericebox/laboratory/internal/names"
)

// Device bootstrap, the controller side (the launcher is package internal/launcher; docs/specs/lab-checks-bootstrap-contract.md).
//
// A device with a bootstrap list, or with a service command of its own, starts the launcher as the first process of its
// container, as root. The launcher places the files, runs the script, erases the list and replaces itself with the service,
// under the service user. The list reaches the container through a memory volume filled by an init container; the Secret
// is mounted only into that init container.
//
// The controller decides, per pod, whether the filesystem is clean and the bootstrap has to run ("run") or the pod starts
// from a saved state ("skip"); the launcher never decides it from the filesystem.

// bootstrapStartupFailureThreshold with the 2 s period gives a startup probe longer than the longest time limit plus the
// pauses between attempts.
const bootstrapStartupFailureThreshold = (labchecks.MaxBootstrapTimeLimit + 300) / readinessPeriodSeconds

// bootstrapMemorySize is the memory volume that holds the list for a moment (the list is at most 512 KiB, a third more in JSON).
var bootstrapMemorySize = resource.MustParse("2Mi")

type launchMode int

const (
	launchNone launchMode = iota // the container runs as the image says
	launchSkip                   // the launcher starts the service only
	launchRun                    // the launcher bootstraps first
)

// podPlan is what the caller of workloadTemplate knows about the pod beyond the Device.
type podPlan struct {
	// Restored: the pod starts from a saved state image, so its filesystem is not clean and the bootstrap is skipped.
	Restored bool
	// Rescue: the pod runs a shell for repair instead of the service.
	Rescue bool
	// Attempt is the number of the bootstrap attempt this pod is for, when the pod is not restarted in place; 0 otherwise.
	Attempt int32
}

func bootstrapEnabled(d *laboratoryv1alpha1.Device) bool {
	return d.Spec.Bootstrap != nil && d.Spec.Bootstrap.Enabled
}

// serviceArgv is the service command of the device spec; empty when the spec does not carry one (the image decides).
func serviceArgv(d *laboratoryv1alpha1.Device) []string {
	if d.Spec.Service == nil {
		return nil
	}
	return append(append([]string(nil), d.Spec.Service.Command...), d.Spec.Service.Args...)
}

// launchModeFor says how the container of a pod starts.
func launchModeFor(d *laboratoryv1alpha1.Device, plan podPlan) launchMode {
	if plan.Rescue || d.Spec.Type != laboratoryv1alpha1.DeviceTypeContainer {
		return launchNone
	}
	switch {
	case bootstrapEnabled(d) && !plan.Restored:
		return launchRun
	case bootstrapEnabled(d) || len(serviceArgv(d)) > 0:
		return launchSkip
	}
	return launchNone
}

// errBootstrapRefused: the device cannot be started as asked. The reason is recorded on the device, no pod is made.
type errBootstrapRefused struct{ reason string }

func (e *errBootstrapRefused) Error() string { return "bootstrap refused: " + e.reason }

// refuseUnbootstrappable fails closed when the controller cannot start the device the way its spec asks: a bootstrap with
// no known service command (nothing to run afterwards), or no launcher image (the node image that carries the program).
func (r *DeviceReconciler) refuseUnbootstrappable(device *laboratoryv1alpha1.Device) error {
	if !bootstrapEnabled(device) {
		return nil
	}
	switch {
	case len(serviceArgv(device)) == 0:
		return &errBootstrapRefused{reason: "the service command is unknown: the image configuration gave none and the device has no command, so nothing can be started after the bootstrap"}
	case r.NetConfigImage == "":
		return &errBootstrapRefused{reason: "this laboratory has no node image to start the bootstrap launcher from"}
	}
	return nil
}

// refusedForGood says the pod was refused for a reason that waiting does not change: an unrunnable profile (an event was
// recorded) or a bootstrap that cannot be started (event and reason in the status).
func (r *DeviceReconciler) refusedForGood(ctx context.Context, device *laboratoryv1alpha1.Device, err error) (bool, error) {
	if stderrors.Is(err, errProfileUnsupported) {
		return true, nil
	}
	return r.handleRefusal(ctx, device, err)
}

// handleRefusal turns a refusal into a status and an event. It reports whether err was one (nothing is retried then).
func (r *DeviceReconciler) handleRefusal(ctx context.Context, device *laboratoryv1alpha1.Device, err error) (bool, error) {
	var refused *errBootstrapRefused
	if !stderrors.As(err, &refused) {
		return false, nil
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(device, corev1.EventTypeWarning, "BootstrapRefused", "%s; the device does not start", refused.reason)
	}
	st := &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapFailed, Reason: clipReason(refused.reason)}
	return true, r.setBootstrapStatus(ctx, device, st)
}

func clipReason(s string) string {
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

// withLauncher turns the main container into a launcher start and, for a bootstrap run, adds the delivery of the list.
func (r *DeviceReconciler) withLauncher(device *laboratoryv1alpha1.Device, spec *corev1.PodSpec, annotations map[string]string, mode launchMode, plan podPlan) {
	argv := serviceArgv(device)
	if mode == launchNone || len(argv) == 0 {
		return
	}
	c := &spec.Containers[0]
	if r.NetConfigImage == "" {
		// No node image to take the launcher from. A bootstrap is refused earlier; a service command alone is applied as is.
		c.Command, c.Args = append([]string(nil), device.Spec.Service.Command...), append([]string(nil), device.Spec.Service.Args...)
		return
	}
	r.ensureProbeInstall(device, spec)

	cmd := []string{names.ProbeBinary, "launch"}
	if mode == launchRun {
		cmd = append(cmd, "-bootstrap")
		if plan.Attempt > 0 {
			cmd = append(cmd, "-attempt", strconv.Itoa(int(plan.Attempt)))
		}
		annotations[names.AnnotationBootstrap] = names.BootstrapRun
		if plan.Attempt > 0 {
			annotations[names.AnnotationBootstrapAttempt] = strconv.Itoa(int(plan.Attempt))
		}
	} else if bootstrapEnabled(device) {
		annotations[names.AnnotationBootstrap] = names.BootstrapSkip
	}
	if device.Spec.Service.User != "" {
		cmd = append(cmd, "-user", device.Spec.Service.User)
	}
	if device.Spec.Service.WorkingDir != "" {
		cmd = append(cmd, "-workdir", device.Spec.Service.WorkingDir)
	}
	c.Command = append(append(cmd, "--"), argv...)
	c.Args = nil

	// The launcher starts as root so that it can place root-owned files, then drops to the service user.
	if c.SecurityContext == nil {
		c.SecurityContext = &corev1.SecurityContext{}
	}
	root := int64(0)
	c.SecurityContext.RunAsUser = &root
	c.SecurityContext.RunAsGroup = &root

	if mode != launchRun {
		return
	}
	spec.Volumes = append(spec.Volumes,
		corev1.Volume{Name: names.BootstrapVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: ptrQuantity(bootstrapMemorySize.DeepCopy()),
		}}},
		corev1.Volume{Name: names.BootstrapSecretVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  device.Name + labchecks.BootstrapSecretSuffix,
			DefaultMode: ptrInt32(0o400),
			Items:       []corev1.KeyToPath{{Key: labchecks.BootstrapKey, Path: labchecks.BootstrapKey}},
			// Not optional: a pod whose list is missing must wait, never start without its bootstrap.
		}}},
	)
	// The Secret is mounted here and nowhere else.
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:            names.BootstrapInitContainer,
		Image:           r.deviceImage(device, r.NetConfigImage),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{names.ProbeImageBinary, "stage", names.BootstrapSrcDir + "/" + labchecks.BootstrapKey, names.BootstrapDir},
		VolumeMounts: []corev1.VolumeMount{
			{Name: names.BootstrapSecretVolume, MountPath: names.BootstrapSrcDir, ReadOnly: true},
			{Name: names.BootstrapVolume, MountPath: names.BootstrapDir},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &root,
			Capabilities:             capsOf(),
			AllowPrivilegeEscalation: ptrBool(false),
			ReadOnlyRootFilesystem:   ptrBool(true),
		},
	})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: names.BootstrapVolume, MountPath: names.BootstrapDir})
	// Started once the launcher has finished with the device (succeeded or gave up): "bootstrap done" for the controller.
	// A startup probe only gates the readiness probe; a failing one is allowed long enough for the longest bootstrap.
	c.StartupProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{names.ProbeBinary, "ready", "-file", names.BootstrapSettledFile}}},
		PeriodSeconds:    readinessPeriodSeconds,
		TimeoutSeconds:   readinessTimeoutSeconds,
		SuccessThreshold: 1,
		FailureThreshold: bootstrapStartupFailureThreshold,
	}
}

// ensureProbeInstall adds the emptyDir with the in-device program, the init container that fills it and the read-only mount
// in the device container, once.
func (r *DeviceReconciler) ensureProbeInstall(device *laboratoryv1alpha1.Device, spec *corev1.PodSpec) {
	for _, v := range spec.Volumes {
		if v.Name == names.ProbeVolume {
			return
		}
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
}

// ---- observed state ----

// bootstrapObservation is what a pod says about the bootstrap of its device.
type bootstrapObservation struct {
	Phase    laboratoryv1alpha1.BootstrapPhase
	Attempts int32
	Reason   string
	// Recycle: the container was restarted in place after the service ended; the pod has to be recreated.
	Recycle bool
}

func bootstrapPod(p *corev1.Pod) bool {
	return p != nil && p.Annotations[names.AnnotationBootstrap] == names.BootstrapRun
}

// observeBootstrap reads the state of the bootstrap of a pod that runs it from the status of its main container: the
// startup probe (the launcher's settled marker) says it finished, the termination message of the last run says how a
// failed attempt ended.
func observeBootstrap(pod *corev1.Pod, container string) bootstrapObservation {
	obs := bootstrapObservation{Phase: laboratoryv1alpha1.BootstrapPending}
	var cs *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == container {
			cs = &pod.Status.ContainerStatuses[i]
		}
	}
	if cs == nil {
		return obs
	}
	// The message of the run that ended last: the current state, else the previous one. A run that ended without a message
	// of the launcher (killed, out of memory) says nothing; the next run counts it as an attempt.
	term := cs.State.Terminated
	if term == nil {
		term = cs.LastTerminationState.Terminated
	}
	var msg launcher.Message
	var haveMsg bool
	if term != nil {
		msg, haveMsg = launcher.ParseMessage(term.Message)
	}
	started := cs.Started != nil && *cs.Started
	switch {
	case haveMsg && msg.Kind == launcher.KindRecycle:
		obs.Phase, obs.Recycle = laboratoryv1alpha1.BootstrapDone, true
	case haveMsg && msg.Kind == launcher.KindFailed && msg.Final:
		obs.Phase, obs.Attempts, obs.Reason = laboratoryv1alpha1.BootstrapFailed, int32(msg.Attempt), clipReason(msg.Reason)
	case started && cs.State.Running != nil:
		obs.Phase = laboratoryv1alpha1.BootstrapDone
		obs.Attempts = 1
		if haveMsg && msg.Kind == launcher.KindFailed {
			obs.Attempts = int32(msg.Attempt) + 1
		}
	case haveMsg && msg.Kind == launcher.KindFailed:
		obs.Attempts, obs.Reason = int32(msg.Attempt), clipReason(msg.Reason)
	}
	return obs
}

func (o bootstrapObservation) status() *laboratoryv1alpha1.DeviceBootstrapStatus {
	return &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: o.Phase, Attempts: o.Attempts, Reason: o.Reason}
}

// deviceBootstrapLaunchPod finds the live pod of a device that is made by a Deployment (it is not owned by the Device).
func (r *DeviceReconciler) liveDevicePod(ctx context.Context, device *laboratoryv1alpha1.Device) *corev1.Pod {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(device.Namespace), client.MatchingLabels{
		names.LabelLab:    device.Spec.LabRef,
		names.LabelDevice: device.Spec.Name,
	}); err != nil {
		return nil
	}
	var best *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best
}

// workloadBootstrap computes the bootstrap status of a device run by a Deployment and recreates a pod whose container was
// restarted in place after its service ended. A Deployment's pod has restartPolicy Always, so the kubelet restarts the
// container, not the pod: the init containers do not run again and the file system is the image's, without the bootstrap.
// Deleting the pod makes the ReplicaSet create a new one, which runs the whole sequence from a clean state. Nil: nothing to
// publish.
func (r *DeviceReconciler) workloadBootstrap(ctx context.Context, device *laboratoryv1alpha1.Device) (*laboratoryv1alpha1.DeviceBootstrapStatus, error) {
	if !bootstrapEnabled(device) {
		return nil, nil
	}
	pod := r.liveDevicePod(ctx, device)
	if pod == nil || !bootstrapPod(pod) {
		return &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapPending}, nil
	}
	obs := observeBootstrap(pod, device.Spec.Name)
	if obs.Recycle {
		if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}
	return obs.status(), nil
}

// setBootstrapStatus writes status.bootstrap when it differs.
func (r *DeviceReconciler) setBootstrapStatus(ctx context.Context, device *laboratoryv1alpha1.Device, st *laboratoryv1alpha1.DeviceBootstrapStatus) error {
	if st == nil || equality.Semantic.DeepEqual(device.Status.Bootstrap, st) {
		return nil
	}
	orig := device.DeepCopy()
	device.Status.Bootstrap = st
	return r.Status().Patch(ctx, device, client.MergeFrom(orig))
}

// devicesForBootstrapPod maps a pod of a bootstrapped device to its Device: the pods of a Deployment are not owned by the
// Device, and the controller has to notice a container that restarted.
func (r *DeviceReconciler) devicesForBootstrapPod(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetAnnotations()[names.AnnotationBootstrap] != names.BootstrapRun {
		return nil
	}
	lab, device := obj.GetLabels()[names.LabelLab], obj.GetLabels()[names.LabelDevice]
	if lab == "" || device == "" {
		return nil
	}
	var out []reconcile.Request
	for _, n := range devices.Candidates(lab, device) {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: n, Namespace: obj.GetNamespace()}})
	}
	return out
}

// ---- devices with state persistence ----

// bootstrapDone says the bootstrap of the device succeeded on the filesystem its saved state comes from.
func bootstrapDone(d *laboratoryv1alpha1.Device) bool {
	return d.Status.Bootstrap != nil && d.Status.Bootstrap.Phase == laboratoryv1alpha1.BootstrapDone
}

func bootstrapFailed(d *laboratoryv1alpha1.Device) bool {
	return bootstrapEnabled(d) && d.Status.Bootstrap != nil && d.Status.Bootstrap.Phase == laboratoryv1alpha1.BootstrapFailed
}

// planForStatePod decides what the next pod of a device with state persistence does about the bootstrap. A saved state is
// restored only when the bootstrap had succeeded on it; otherwise it is discarded (a snapshot of a half-bootstrapped
// filesystem must never become the device), and the pod starts from the base image with the next attempt number.
func planForStatePod(d *laboratoryv1alpha1.Device, hasImage, rescue bool) (plan podPlan, discard bool) {
	plan.Rescue = rescue
	if !bootstrapEnabled(d) {
		plan.Restored = hasImage
		return plan, false
	}
	if hasImage && (bootstrapDone(d) || rescue) {
		plan.Restored = true
		return plan, false
	}
	plan.Attempt = 1
	if bs := d.Status.Bootstrap; bs != nil && bs.Phase == laboratoryv1alpha1.BootstrapPending {
		plan.Attempt = bs.Attempts + 1 // Attempts counts the failed ones so far
	}
	return plan, hasImage
}

// recordEndedBootstrapPod notes how a pod of a device with state persistence ended, when it ended before the bootstrap
// finished: one more failed attempt, the last one fails the device. The attempt number is the one the pod was created for,
// so counting the same pod twice changes nothing.
func (r *DeviceReconciler) recordEndedBootstrapPod(ctx context.Context, device *laboratoryv1alpha1.Device, pod *corev1.Pod) error {
	if !bootstrapEnabled(device) || !bootstrapPod(pod) {
		return nil
	}
	if bootstrapDone(device) {
		return nil // the bootstrap of this pod had succeeded (a new run pod resets the status first): it ended for another reason
	}
	obs := observeBootstrap(pod, device.Spec.Name)
	attempt, _ := strconv.Atoi(pod.Annotations[names.AnnotationBootstrapAttempt])
	if attempt <= 0 {
		attempt = 1
	}
	reason := obs.Reason
	if reason == "" {
		reason = "the container ended during the bootstrap"
	}
	st := &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapPending, Attempts: int32(attempt), Reason: clipReason(reason)}
	if attempt >= labchecks.BootstrapMaxAttempts {
		st.Phase = laboratoryv1alpha1.BootstrapFailed
	}
	return r.setBootstrapStatus(ctx, device, st)
}

// syncStateBootstrap publishes the bootstrap status of the current pod of a device with state persistence.
func (r *DeviceReconciler) syncStateBootstrap(ctx context.Context, device *laboratoryv1alpha1.Device, pod *corev1.Pod) error {
	if !bootstrapEnabled(device) || !bootstrapPod(pod) {
		return nil // a pod that skips the bootstrap says nothing new: the state it restored had finished it
	}
	obs := observeBootstrap(pod, device.Spec.Name)
	if cur := device.Status.Bootstrap; obs.Phase == laboratoryv1alpha1.BootstrapPending && obs.Attempts == 0 && cur != nil && cur.Phase == laboratoryv1alpha1.BootstrapPending {
		return nil // the pod has not said anything yet: the count of the earlier attempts stays
	}
	return r.setBootstrapStatus(ctx, device, obs.status())
}
