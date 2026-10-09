package laboratory

// Serialized lifecycle, scope, and ownership values remain compatible with
// existing native reports and persisted API objects.
const (
	runtimeStateAllocated      = "Allocated"
	retirementStateDeleted     = "Deleted"
	lifecycleStateRunning      = "Running"
	ownerKindLab               = "Lab"
	groupUIDEnvironment        = "GROUP_UID"
	scopeKindLabFabric         = "LabFabric"
	ownerKindDevice            = "Device"
	snapshotModeRequired       = "Required"
	runtimeStateReleased       = "Released"
	scopeKindNeverMaterialized = "NeverMaterialized"
	observationStateUnknown    = "Unknown"
)
