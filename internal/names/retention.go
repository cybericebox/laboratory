package names

// AnnotationSnapshotRetirement is an irreversible repository-retirement fence,
// written at the accepted deadline only after exact stopped/released observation.
const AnnotationSnapshotRetirement = LabelPrefix + "snapshot-retirement"

// AnnotationLifecycleRetirement is a permanent fenced tombstone intent. It is
// separate from expiry-driven snapshot-only retention and blocks future Start.
const AnnotationLifecycleRetirement = "laboratory.cybericebox.com/lifecycle-retirement"

// AnnotationLabVariableAdmission serializes write-only Secret updates against
// permanent retirement. It stores only a SHA256 retry token, never values.
const AnnotationLabVariableAdmission = "laboratory.cybericebox.com/variable-admission"

const AnnotationLabCreation = "laboratory.cybericebox.com/creation-receipt"
