package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// bootstrapLists is device name -> bootstrap list. A listed device stays in the map even when its list is empty (an Update
// reads that as "remove the Secret").
type bootstrapLists map[string]*labchecks.Bootstrap

// checkSet is the checks of one lab. A nil *checkSet means "not given"; a set with no checks is "given and empty".
type checkSet struct{ checks []labchecks.Check }

// maxTerminalCommandItems and the limits of the service fields bound what a device template may carry.
const (
	maxTerminalCommandItems = 32
	maxServiceItems         = 256
	maxServiceItemBytes     = 4096
)

// ---- proto <-> labchecks ---------------------------------------------------------------------------------------------

func bootstrapFromProto(in *protobuf.DeviceBootstrap) *labchecks.Bootstrap {
	b := &labchecks.Bootstrap{Script: in.GetScript(), Interpreter: in.GetInterpreter(), TimeLimitSeconds: in.GetTimeLimitSeconds()}
	for _, f := range in.GetFiles() {
		b.Files = append(b.Files, labchecks.File{Path: f.GetPath(), Content: append([]byte(nil), f.GetContent()...), Owner: f.GetOwner(), Group: f.GetGroup(), Mode: f.GetMode()})
	}
	return b
}

// mergeBootstrapLists folds DeviceBootstrap lists into one map; a later list replaces an earlier one device by device.
func mergeBootstrapLists(lists ...[]*protobuf.DeviceBootstrap) bootstrapLists {
	out := bootstrapLists{}
	for _, list := range lists {
		for _, db := range list {
			if db == nil {
				continue
			}
			out[db.GetDevice()] = bootstrapFromProto(db)
		}
	}
	return out
}

func checksFromProto(in *protobuf.LabChecks) *checkSet {
	if in == nil {
		return nil
	}
	set := &checkSet{}
	for _, c := range in.GetItems() {
		ck := labchecks.Check{ID: c.GetId(), Device: c.GetDevice(), ScheduleSeconds: c.GetScheduleSeconds(), ManualRun: c.GetManualRun(), TimeLimitSeconds: c.GetTimeLimitSeconds()}
		switch c.GetMode() {
		case protobuf.CheckMode_CHECK_MODE_ONCE:
			ck.Mode = "once"
		case protobuf.CheckMode_CHECK_MODE_CONTINUOUS:
			ck.Mode = "continuous"
		}
		for _, p := range c.GetParts() {
			var part labchecks.Part
			if f := p.GetFile(); f != nil {
				part.File = &labchecks.FilePart{Path: f.GetPath(), Exists: f.Exists, ContentContains: f.ContentContains, ContentMatches: f.ContentMatches, Mode: f.Mode, Owner: f.Owner, Group: f.Group}
			}
			if cu := p.GetCustom(); cu != nil {
				part.Custom = &labchecks.CustomPart{Command: append([]string(nil), cu.GetCommand()...), Script: cu.GetScript(), Interpreter: cu.GetInterpreter(), User: cu.GetUser(), OutputContains: cu.OutputContains, OutputMatches: cu.OutputMatches}
			}
			ck.Parts = append(ck.Parts, part)
		}
		set.checks = append(set.checks, ck)
	}
	return set
}

// pickChecks: the item's own set replaces the variant's as a whole.
func pickChecks(variant, item *protobuf.LabChecks) *checkSet {
	if item != nil {
		return checksFromProto(item)
	}
	return checksFromProto(variant)
}

func (s *checkSet) empty() bool { return s == nil || len(s.checks) == 0 }

// ---- validation ------------------------------------------------------------------------------------------------------

// containerDevices is the set of devices that can carry a bootstrap, a check or a terminal.
func containerDevices(spec *laboratoryv1alpha1.LabSpec) map[string]bool {
	out := map[string]bool{}
	for i := range spec.Devices {
		if spec.Devices[i].Type == laboratoryv1alpha1.DeviceTypeContainer {
			out[spec.Devices[i].Name] = true
		}
	}
	return out
}

// validateBootstrapLists normalizes and checks the lists against the devices of the spec. A list without script and files is
// kept (it removes the Secret) and not checked further.
func validateBootstrapLists(lists bootstrapLists, spec *laboratoryv1alpha1.LabSpec) error {
	devices := containerDevices(spec)
	all := specDevices(spec)
	for _, dev := range sortedKeys(lists) {
		if !all[dev] {
			return fmt.Errorf("bootstrap for unknown device %q", dev)
		}
		if !devices[dev] {
			return fmt.Errorf("device %q: only a container device can have a bootstrap", dev)
		}
		b := lists[dev]
		if b.Empty() {
			continue
		}
		labchecks.NormalizeBootstrap(b)
		if err := labchecks.ValidateBootstrap(b); err != nil {
			return fmt.Errorf("device %q: bootstrap: %w", dev, err)
		}
	}
	return nil
}

// validateCheckSet normalizes and checks the checks against the devices of the spec.
func validateCheckSet(set *checkSet, spec *laboratoryv1alpha1.LabSpec) error {
	if set == nil {
		return nil
	}
	all := specDevices(spec)
	for i := range set.checks {
		if d := set.checks[i].Device; d != "" && all[d] && !containerDevices(spec)[d] {
			return fmt.Errorf("check %q: only a container device can be checked, %q is not one", set.checks[i].ID, d)
		}
	}
	labchecks.NormalizeChecks(set.checks)
	if err := labchecks.ValidateChecks(set.checks, containerDevices(spec)); err != nil {
		return err
	}
	if _, err := labchecks.MarshalChecks(set.checks); err != nil {
		return err
	}
	return nil
}

// validateDeviceExtras checks the non-secret service and terminal fields of every device of a spec.
func validateDeviceExtras(spec *laboratoryv1alpha1.LabSpec) error {
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if s := d.Service; s != nil {
			if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
				return fmt.Errorf("device %q: only a container device has a service command", d.Name)
			}
			if len(s.Command) > maxServiceItems || len(s.Args) > maxServiceItems {
				return fmt.Errorf("device %q: service command or args have more than %d items", d.Name, maxServiceItems)
			}
			for _, item := range append(append([]string(nil), s.Command...), s.Args...) {
				if len(item) > maxServiceItemBytes || strings.ContainsRune(item, 0) {
					return fmt.Errorf("device %q: a service command item is over %d bytes or contains NUL", d.Name, maxServiceItemBytes)
				}
			}
			if len(s.Command) > 0 && s.Command[0] == "" {
				return fmt.Errorf("device %q: the service command is empty", d.Name)
			}
			if s.WorkingDir != "" && (!strings.HasPrefix(s.WorkingDir, "/") || strings.ContainsRune(s.WorkingDir, 0)) {
				return fmt.Errorf("device %q: service workingDir %q is not an absolute path", d.Name, s.WorkingDir)
			}
			if len(s.User) > 128 || strings.ContainsAny(s.User, "\x00\n") {
				return fmt.Errorf("device %q: invalid service user", d.Name)
			}
		}
		if t := d.Terminal; t != nil && t.Enabled {
			if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
				return fmt.Errorf("device %q: only a container device can have a terminal", d.Name)
			}
			if len(t.Command) > maxTerminalCommandItems {
				return fmt.Errorf("device %q: the terminal command has more than %d items", d.Name, maxTerminalCommandItems)
			}
			for j, item := range t.Command {
				if (j == 0 && item == "") || len(item) > maxServiceItemBytes || strings.ContainsRune(item, 0) {
					return fmt.Errorf("device %q: invalid terminal command", d.Name)
				}
			}
			if len(t.User) > 128 || strings.ContainsAny(t.User, "\x00\n") {
				return fmt.Errorf("device %q: invalid terminal user", d.Name)
			}
		}
	}
	return nil
}

// requireTerminal refuses a spec with a terminal device when the laboratory's terminal option is off.
func (h *Handler) requireTerminal(spec *laboratoryv1alpha1.LabSpec) error {
	if !h.capabilitiesEnforced || h.features.TerminalEnabled {
		return nil
	}
	for i := range spec.Devices {
		if t := spec.Devices[i].Terminal; t != nil && t.Enabled {
			return fmt.Errorf("device %q asks for a terminal, but the terminal option is off on this laboratory (features.terminal.enabled is false)", spec.Devices[i].Name)
		}
	}
	return nil
}

// requireChecks refuses checks when the laboratory's checks option is off.
func (h *Handler) requireChecks(set *checkSet) error {
	if set.empty() || !h.capabilitiesEnforced || h.checksAvailable() {
		return nil
	}
	return fmt.Errorf("the lab carries checks, but %s", h.checksUnavailableText())
}

// ---- stamping the non-secret part into the CR spec -------------------------------------------------------------------

// stampContract sets the non-secret markers of the bootstrap lists and checks on a spec the agent is about to create. What a
// client put there is replaced: only the agent says that a Secret exists.
func stampContract(spec *laboratoryv1alpha1.LabSpec, lists bootstrapLists, set *checkSet) {
	for i := range spec.Devices {
		spec.Devices[i].Bootstrap = nil
		if b := lists[spec.Devices[i].Name]; !b.Empty() {
			spec.Devices[i].Bootstrap = &laboratoryv1alpha1.DeviceBootstrapSpec{Enabled: true}
		}
	}
	spec.Checks = nil
	if set.empty() {
		return
	}
	refs := make([]laboratoryv1alpha1.CheckRef, 0, len(set.checks))
	for _, c := range set.checks {
		refs = append(refs, laboratoryv1alpha1.CheckRef{ID: c.ID, Device: c.Device, ManualRun: c.ManualRun})
	}
	spec.Checks = &laboratoryv1alpha1.LabChecksSpec{Items: refs}
}

// clearStamped drops the markers a client sent in spec_json.
func clearStamped(spec *laboratoryv1alpha1.LabSpec) {
	spec.Checks = nil
	for i := range spec.Devices {
		spec.Devices[i].Bootstrap = nil
	}
}

// ---- Secrets ---------------------------------------------------------------------------------------------------------

// labSecrets is what one write puts into the write-only Secrets of a lab: device variables, device bootstrap lists, and the
// checks of the lab (nil = leave as they are).
type labSecrets struct {
	devices   []string
	env       deviceVars
	bootstrap bootstrapLists
	checks    *checkSet
}

func (s labSecrets) token(parent *laboratoryv1alpha1.Lab) string {
	if len(s.bootstrap) == 0 && s.checks == nil {
		return variableWriteToken(parent, s.devices, s.env)
	}
	var checks []labchecks.Check
	if s.checks != nil {
		checks = s.checks.checks
	}
	raw, _ := json.Marshal(struct {
		Base        string
		Bootstrap   bootstrapLists
		ChecksGiven bool
		Checks      []labchecks.Check
	}{variableWriteToken(parent, s.devices, s.env), s.bootstrap, s.checks != nil, checks})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// writeSecretDoc replaces one Secret: delete (a missing one is fine), then create when there is a document.
func (h *Handler) writeSecretDoc(ctx context.Context, lab *laboratoryv1alpha1.Lab, name, device, key string, doc []byte) error {
	secrets := h.k8s.CoreV1().Secrets(lab.Namespace)
	if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if doc == nil {
		return nil
	}
	labels := map[string]string{names.LabelLab: lab.Name}
	if device != "" {
		labels[names.LabelDevice] = device
	}
	_, err := secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lab.Namespace, OwnerReferences: []metav1.OwnerReference{labOwner(lab)}, Labels: labels},
		Data:       map[string][]byte{key: doc},
	}, metav1.CreateOptions{})
	return err
}

// writeBootstrapSecrets rewrites the Secret <device object>-bootstrap of every listed device: written when the list has a
// script or files, removed otherwise. Like the -env Secret it is only ever created and deleted by the agent, never read.
func (h *Handler) writeBootstrapSecrets(ctx context.Context, lab *laboratoryv1alpha1.Lab, lists bootstrapLists) error {
	for _, dev := range sortedKeys(lists) {
		objName, err := h.deviceObjectName(ctx, lab.Namespace, lab.Name, dev)
		if err != nil {
			return err
		}
		var doc []byte
		if b := lists[dev]; !b.Empty() {
			if doc, err = labchecks.MarshalBootstrap(b); err != nil {
				return err
			}
		}
		if err := h.writeSecretDoc(ctx, lab, objName+labchecks.BootstrapSecretSuffix, dev, labchecks.BootstrapKey, doc); err != nil {
			return err
		}
	}
	return nil
}

// writeChecksSecret rewrites the Secret <lab>-checks.
func (h *Handler) writeChecksSecret(ctx context.Context, lab *laboratoryv1alpha1.Lab, set *checkSet) error {
	if set == nil {
		return nil
	}
	var doc []byte
	if len(set.checks) > 0 {
		var err error
		if doc, err = labchecks.MarshalChecks(set.checks); err != nil {
			return err
		}
	}
	return h.writeSecretDoc(ctx, lab, lab.Name+labchecks.ChecksSecretSuffix, "", labchecks.ChecksKey, doc)
}

// ---- Update ----------------------------------------------------------------------------------------------------------

// checkUpdateTargets refuses what the immutable CR spec cannot follow: a bootstrap list for a device that had none at
// creation, or checks whose ids, devices or manual-run flags differ from the lab's.
func checkUpdateTargets(lab *laboratoryv1alpha1.Lab, lists bootstrapLists, set *checkSet) error {
	for _, dev := range sortedKeys(lists) {
		if lists[dev].Empty() {
			continue
		}
		var declared bool
		for i := range lab.Spec.Devices {
			if d := &lab.Spec.Devices[i]; d.Name == dev && d.Bootstrap != nil && d.Bootstrap.Enabled {
				declared = true
			}
		}
		if !declared {
			return fmt.Errorf("device %q had no bootstrap when the lab was created; a bootstrap cannot be added later", dev)
		}
	}
	if set == nil {
		return nil
	}
	if lab.Spec.Checks == nil {
		return fmt.Errorf("the lab had no checks when it was created; checks cannot be added later")
	}
	have := map[string]laboratoryv1alpha1.CheckRef{}
	for _, r := range lab.Spec.Checks.Items {
		have[r.ID] = r
	}
	if len(set.checks) != len(have) {
		return fmt.Errorf("the checks must keep the %d ids of the lab (got %d)", len(have), len(set.checks))
	}
	for _, c := range set.checks {
		r, ok := have[c.ID]
		if !ok || r.Device != c.Device || r.ManualRun != c.ManualRun {
			return fmt.Errorf("check %q: the id, device and manual_run cannot change after creation", c.ID)
		}
	}
	return nil
}

// ---- RunChecks -------------------------------------------------------------------------------------------------------

// runChecksRequest is the value of the annotation names.AnnotationRunChecks.
type runChecksRequest struct {
	// Token grows by one with every accepted request; the checker acknowledges it in status.checksRunAck.
	Token int64 `json:"token"`
	// IDs are the checks to run; empty = every check that allows a manual run.
	IDs []string `json:"ids,omitempty"`
}

// RunChecks records a manual run of checks. Acceptance only: the result of a check comes in LabStatus.checks.
func (h *Handler) RunChecks(ctx context.Context, in *protobuf.RunChecksRequest) (*protobuf.BatchResult, error) {
	if h.capabilitiesEnforced && !h.checksAvailable() {
		return nil, status.Error(codes.FailedPrecondition, h.checksUnavailableText())
	}
	items := in.GetItems()
	if len(items) == 0 {
		return nil, invalid("give items")
	}
	if err := checkItemCount(len(items)); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(items))
	for i, it := range items {
		refs[i] = nsRef(it.GetLabGroup(), it.GetLab())
		if err := names.ValidateID(it.GetLab()); err != nil {
			return nil, invalid("item %d: lab: %v", i, err)
		}
		if err := names.ValidateID(it.GetLabGroup()); err != nil {
			return nil, invalid("item %d: lab_group: %v", i, err)
		}
		if len(it.GetCheckIds()) > labchecks.MaxChecksPerLab {
			return nil, invalid("item %d (%s): more than %d check ids", i, describeRef(refs[i]), labchecks.MaxChecksPerLab)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		if err := h.recordRunChecks(ctx, resolver, refs[i], items[i].GetCheckIds()); err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}

func (h *Handler) recordRunChecks(ctx context.Context, resolver *groupResolver, ref *protobuf.ItemRef, ids []string) error {
	ns, err := resolver.namespace(ctx, ref.GetLabGroup())
	if err != nil {
		return err
	}
	labs := h.cs.LaboratoryV1alpha1().Labs(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := labs.Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindLab, cur); err != nil {
			return err
		}
		if cur.Annotations[names.AnnotationLifecycleRetirement] != "" {
			return fmt.Errorf("retired Lab cannot run checks")
		}
		want, err := selectCheckIDs(cur, ids)
		if err != nil {
			return invalid("%v", err)
		}
		next := runChecksRequest{IDs: want}
		var prev runChecksRequest
		if raw := cur.Annotations[names.AnnotationRunChecks]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &prev)
		}
		next.Token = max(prev.Token, cur.Status.ChecksRunAck) + 1
		if prev.Token > cur.Status.ChecksRunAck {
			// the checker has not taken the previous request: do not lose it
			next.IDs = mergeIDs(prev.IDs, want)
		}
		raw, _ := json.Marshal(next)
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations[names.AnnotationRunChecks] = string(raw)
		_, err = labs.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// selectCheckIDs validates the requested ids against the checks of the lab; none requested stays none (all manual-run checks).
func selectCheckIDs(lab *laboratoryv1alpha1.Lab, ids []string) ([]string, error) {
	if lab.Spec.Checks == nil || len(lab.Spec.Checks.Items) == 0 {
		return nil, fmt.Errorf("the lab has no checks")
	}
	refs := map[string]laboratoryv1alpha1.CheckRef{}
	manual := 0
	for _, r := range lab.Spec.Checks.Items {
		refs[r.ID] = r
		if r.ManualRun {
			manual++
		}
	}
	if len(ids) == 0 {
		if manual == 0 {
			return nil, fmt.Errorf("no check of the lab allows a manual run")
		}
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		r, ok := refs[id]
		switch {
		case !ok:
			return nil, fmt.Errorf("unknown check %q", id)
		case !r.ManualRun:
			return nil, fmt.Errorf("check %q does not allow a manual run", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// mergeIDs unites two id lists where an empty list means "all": the union of anything with "all" is "all".
func mergeIDs(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range append(append([]string(nil), a...), b...) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ---- results to proto ------------------------------------------------------------------------------------------------

func bootstrapStatusToProto(b *laboratoryv1alpha1.DeviceBootstrapStatus) *protobuf.BootstrapStatus {
	if b == nil {
		return nil
	}
	out := &protobuf.BootstrapStatus{Attempts: b.Attempts, Reason: labchecks.TruncateDetail(b.Reason)}
	if len(out.Reason) > 256 {
		out.Reason = out.Reason[:256]
	}
	switch b.Phase {
	case laboratoryv1alpha1.BootstrapPending:
		out.Phase = protobuf.BootstrapPhase_BOOTSTRAP_PHASE_PENDING
	case laboratoryv1alpha1.BootstrapDone:
		out.Phase = protobuf.BootstrapPhase_BOOTSTRAP_PHASE_DONE
	case laboratoryv1alpha1.BootstrapFailed:
		out.Phase = protobuf.BootstrapPhase_BOOTSTRAP_PHASE_FAILED
	}
	return out
}

func checkResultsToProto(in []laboratoryv1alpha1.CheckResult) []*protobuf.CheckResult {
	if len(in) == 0 {
		return nil
	}
	out := make([]*protobuf.CheckResult, 0, len(in))
	for i := range in {
		r := &in[i]
		p := &protobuf.CheckResult{Id: r.ID, FirstPassedUnixMs: r.FirstPassedMs, ChangedUnixMs: r.ChangedMs, Value: labchecks.TruncateValue(r.Value), Detail: labchecks.TruncateDetail(r.Detail)}
		switch r.State {
		case laboratoryv1alpha1.CheckPassed:
			p.State = protobuf.CheckState_CHECK_STATE_PASSED
		case laboratoryv1alpha1.CheckNotPassed:
			p.State = protobuf.CheckState_CHECK_STATE_NOT_PASSED
		case laboratoryv1alpha1.CheckError:
			p.State = protobuf.CheckState_CHECK_STATE_ERROR
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// checksFeature and bootstrapFeature are the parts of GetFeatures for the contract.
func (h *Handler) checksFeature() *protobuf.ChecksFeature {
	return &protobuf.ChecksFeature{
		Enabled: h.checksAvailable(), MaxChecksPerLab: labchecks.MaxChecksPerLab, MaxPartsPerCheck: labchecks.MaxPartsPerCheck,
		MaxTimeLimitSeconds: labchecks.MaxCheckTimeLimit, MaxScheduleSeconds: labchecks.MaxScheduleSeconds, MaxDetailBytes: labchecks.MaxResultDetailBytes,
	}
}

func bootstrapFeature() *protobuf.BootstrapFeature {
	return &protobuf.BootstrapFeature{
		MaxScriptBytes: labchecks.MaxScriptBytes, MaxFiles: labchecks.MaxBootstrapFiles, MaxFileBytes: labchecks.MaxFileBytes,
		MaxTotalBytes: labchecks.MaxBootstrapBytes, MaxTimeLimitSeconds: labchecks.MaxBootstrapTimeLimit, MaxAttempts: labchecks.BootstrapMaxAttempts,
	}
}
