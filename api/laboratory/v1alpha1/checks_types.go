package v1alpha1

// The contract of device bootstrap, lab checks and the device terminal (docs/specs/lab-checks-bootstrap-contract.md).
//
// Secret material never lives on a custom resource: the bootstrap list of a device is in the Secret <device>-bootstrap and
// the check definitions of a lab are in the Secret <lab>-checks, both written by the management agent and never read back.
// The CR carries only what is not secret: the switches, the names and the observed state.

// DeviceService is the service command of a container device, taken by the platform from the image configuration (the
// author may override the command). An empty field means "not provided": the runtime then uses what the image says.
type DeviceService struct {
	// Command is the entrypoint list.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=4096
	// +optional
	Command []string `json:"command,omitempty"`
	// Args are the arguments of the command.
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=4096
	// +optional
	Args []string `json:"args,omitempty"`
	// User is the user (name or uid[:gid]) the service runs as.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	User string `json:"user,omitempty"`
	// WorkingDir is the working directory of the service (an absolute path).
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`
}

// DeviceTerminal enables the browser terminal of a device. Only the data model: the component that serves it comes later.
type DeviceTerminal struct {
	// Enabled turns the terminal on. A lab with a terminal is refused by a laboratory whose terminal option is off.
	Enabled bool `json:"enabled,omitempty"`
	// Command starts the terminal session; empty means /bin/sh.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=4096
	// +optional
	Command []string `json:"command,omitempty"`
	// User the session runs as; empty means the user of the service.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	User string `json:"user,omitempty"`
}

// DeviceBootstrapSpec says that the device has a bootstrap list in the Secret <device>-bootstrap. The management agent sets
// it (from the list it received); a value in a client's spec is ignored. The list, with its time limit, is never on the CR.
type DeviceBootstrapSpec struct {
	// Enabled: the Secret exists (or is being written) and the service must not start before the bootstrap is done.
	Enabled bool `json:"enabled,omitempty"`
}

// BootstrapPhase is where the bootstrap of a device is.
// +kubebuilder:validation:Enum=none;pending;done;failed
type BootstrapPhase string

const (
	// BootstrapNone: the device has no bootstrap, or nothing observed yet.
	BootstrapNone BootstrapPhase = "none"
	// BootstrapPending: not finished (not started, running, or waiting for the next attempt).
	BootstrapPending BootstrapPhase = "pending"
	// BootstrapDone: succeeded.
	BootstrapDone BootstrapPhase = "done"
	// BootstrapFailed: the attempts are used up; the device stays in this state with the reason for the author.
	BootstrapFailed BootstrapPhase = "failed"
)

// DeviceBootstrapStatus is the observed bootstrap of a device. It holds no secret: the reason is a short text for the author
// and must not repeat file contents or script output.
type DeviceBootstrapStatus struct {
	Phase BootstrapPhase `json:"phase,omitempty"`
	// Attempts is the number of attempts made so far (at most three, internal/labchecks.BootstrapMaxAttempts).
	Attempts int32 `json:"attempts,omitempty"`
	// Reason is a short explanation of a failed or pending bootstrap (at most 256 bytes).
	// +kubebuilder:validation:MaxLength=256
	Reason string `json:"reason,omitempty"`
}

// CheckMode is how often a check runs and what its result means.
// +kubebuilder:validation:Enum=once;continuous
type CheckMode string

const (
	// CheckOnce stops after the first pass.
	CheckOnce CheckMode = "once"
	// CheckContinuous: the current state matters.
	CheckContinuous CheckMode = "continuous"
)

// CheckState is the result of a check.
// +kubebuilder:validation:Enum=unknown;passed;not_passed;error
type CheckState string

const (
	// CheckUnknown: not run yet.
	CheckUnknown CheckState = "unknown"
	// CheckPassed: every part held in the same run.
	CheckPassed CheckState = "passed"
	// CheckNotPassed: a part did not hold.
	CheckNotPassed CheckState = "not_passed"
	// CheckError: the check could not be made (device not ready, command did not start or ran over its limit). The previous
	// state is kept by the consumer, which reads the state of the previous change.
	CheckError CheckState = "error"
)

// LabChecksSpec says that the lab has checks and lists them without anything secret. The definitions (observations and
// expected values) are in the Secret <lab>-checks. The management agent sets it; a value in a client's spec is ignored.
type LabChecksSpec struct {
	// +listType=map
	// +listMapKey=id
	// +kubebuilder:validation:MaxItems=64
	Items []CheckRef `json:"items,omitempty"`
}

// CheckRef is the part of a check that holds no expected value.
type CheckRef struct {
	// ID is the id of the check, unique in the lab.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	ID string `json:"id"`
	// Device is the device the check observes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=35
	Device string `json:"device"`
	// ManualRun: the check may be run by a RunChecks call.
	ManualRun bool `json:"manualRun,omitempty"`
}

// CheckResult is the last published result of one check of a lab. The checker patches only the entries that changed.
type CheckResult struct {
	ID    string     `json:"id"`
	State CheckState `json:"state"`
	// FirstPassedMs is the time of the first pass, unix milliseconds; 0 = never passed.
	FirstPassedMs int64 `json:"firstPassedMs,omitempty"`
	// ChangedMs is the time the state last changed, unix milliseconds.
	ChangedMs int64 `json:"changedMs,omitempty"`
	// Value is an optional value of the check (needed later for king-of-the-hill).
	// +kubebuilder:validation:MaxLength=256
	Value string `json:"value,omitempty"`
	// Detail is a short text for the author or the teacher, never for the participant (at most 2048 bytes).
	// +kubebuilder:validation:MaxLength=2048
	Detail string `json:"detail,omitempty"`
}
