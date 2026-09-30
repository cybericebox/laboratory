package names

const (
	// AnnotationStateEpoch is set on a snapshot-backed device pod: the reset
	// epoch it was created in. The node-agent ignores snapshots of pods whose
	// epoch is older than the device's current one.
	AnnotationStateEpoch = "state.cybericebox.com/epoch"
	// AnnotationStateRescue is "true" on a device pod started in rescue mode.
	AnnotationStateRescue = "state.cybericebox.com/rescue"
	// AnnotationStateDevice is set on a snapshot-backed device pod and holds
	// the name of the Device CR, so the node-agent finds the policy and the
	// status without parsing pod names.
	AnnotationStateDevice = "state.cybericebox.com/device"
	// AnnotationStateIncarnation is the incarnation number of a snapshot-backed
	// device pod; the node-agent records snapshots only for the current one.
	AnnotationStateIncarnation = "state.cybericebox.com/incarnation"
)
