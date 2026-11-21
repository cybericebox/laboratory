package names

// Device bootstrap (see package internal/launcher). The list reaches the device only through a memory-backed volume that
// the launcher empties before the service starts; the Secret that holds it is mounted into an init container, never into
// the device container.
const (
	// BootstrapInitContainer copies the list from the Secret into the memory volume. BootstrapSecretVolume is the Secret,
	// mounted only there, at BootstrapSrcDir.
	BootstrapInitContainer = "cib-bootstrap"
	BootstrapSecretVolume  = "cib-bootstrap-src"
	BootstrapSrcDir        = "/.cib/src"
	// BootstrapVolume is the memory-backed emptyDir; the device container sees it at BootstrapDir.
	BootstrapVolume = "cib-bootstrap"
	BootstrapDir    = "/.cib/boot"
	// BootstrapSettledFile exists once the launcher has finished with the device: it succeeded or gave up. It is the
	// startup probe. BootstrapOKFile exists only after success and is part of the readiness probe.
	BootstrapSettledFile = BootstrapDir + "/settled"
	BootstrapOKFile      = BootstrapDir + "/ok"

	// AnnotationBootstrap on a device pod is the controller's decision for that pod: "run" (the launcher bootstraps a clean
	// filesystem) or "skip" (the pod starts from a saved state, the launcher only starts the service).
	AnnotationBootstrap = LabelPrefix + "bootstrap"
	// AnnotationBootstrapAttempt is the attempt number a pod of a device with state persistence was created for.
	AnnotationBootstrapAttempt = LabelPrefix + "bootstrap-attempt"
	// BootstrapRun and BootstrapSkip are the values of AnnotationBootstrap.
	BootstrapRun  = "run"
	BootstrapSkip = "skip"
)
