package grpc

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const kindLabGroup = "LabGroup"

func groupRef(name string) *protobuf.ItemRef { return &protobuf.ItemRef{Name: name} }

// CreateLabGroups creates LabGroup custom resources. Re-sending an item whose group
// exists with the same spec is EXISTS (labels it lacks are added: UPDATED); a group with
// a different spec is FAILED for that item.
func (h *Handler) CreateLabGroups(ctx context.Context, in *protobuf.CreateLabGroupsRequest) (*protobuf.BatchResult, error) {
	items := in.GetItems()
	if err := checkItemCount(len(items)); err != nil {
		return nil, err
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, invalid("%v", err)
	}
	refs := make([]*protobuf.ItemRef, len(items))
	for i, it := range items {
		refs[i] = groupRef(it.GetName())
		if err := names.ValidateID(it.GetName()); err != nil {
			return nil, invalid("item %d: %v", i, err)
		}
		if err := validateLabels(it.GetLabels()); err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
		}
		if _, err := newDeploySpec(it.GetDeployGroup(), it.GetDeployAfter()); err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
		}
		if _, _, err := groupSwitches(it); err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
		}
		computed, err := h.applyExpectedCounts(it)
		if err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
		}
		// sizes the agent computed from the counts are within its own limits; the sizes a caller sent are checked
		if !computed {
			if err := h.checkPodSizes(it); err != nil {
				return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
			}
		}
		if err := reservationIDError(it.GetReservationId()); err != nil {
			return nil, invalid("item %d (%s): %v", i, it.GetName(), err)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	fit := make([]fitItem, len(items))
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.GetReservationId()
		lg := &laboratoryv1alpha1.LabGroup{}
		lg.Spec.VPN.Enabled, lg.Spec.Gateway.Enabled, _ = h.clusterSwitchesOf(it)
		lg.Spec.VPN.Size, lg.Spec.Gateway.Size = podSize(it.GetVpnSize()), podSize(it.GetGatewaySize())
		fit[i] = fitItem{reservation: it.GetReservationId(), need: h.groupNeedOf(lg), deployGroup: names.DeployKey(it.GetDeployGroup())}
		if _, err := h.getGroup(ctx, it.GetName()); err == nil {
			fit[i].exists = true
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	// held from reading what the reservations hold to the last create, so two calls of this agent cannot both take the rest
	defer h.lockReservations(ids)()
	refusals, err := h.reservationRefusals(ctx, tenantOf(ctx), fit)
	if err != nil {
		return nil, err
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		if refusals[i] != nil {
			return failedResult(refs[i], refusals[i])
		}
		res := h.createLabGroup(ctx, items[i], in.GetLabels())
		if res.GetState() != protobuf.ItemState_ITEM_STATE_FAILED {
			// the sizes the counts became (applyExpectedCounts), for the caller to see
			res.VpnSize, res.GatewaySize = items[i].GetVpnSize(), items[i].GetGatewaySize()
		}
		return res
	})}, nil
}

func (h *Handler) createLabGroup(ctx context.Context, it *protobuf.LabGroupItem, common map[string]string) *protobuf.ItemResult {
	ref := groupRef(it.GetName())
	name := crName(it.GetName())
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	want := mergeItemLabels(common, it.GetLabels())
	dep, _ := newDeploySpec(it.GetDeployGroup(), it.GetDeployAfter())
	lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
	lg.Spec.Suspended = it.GetSuspended()
	var refused error
	if lg.Spec.VPN.Enabled, lg.Spec.Gateway.Enabled, refused = h.clusterSwitchesOf(it); refused != nil {
		return failedResult(ref, refused)
	}
	lg.Spec.VPN.Disabled = !*lg.Spec.VPN.Enabled // the old switch says the same, for whatever still reads it
	lg.Spec.VPN.ProbeWhileSuspended = it.GetProbeWhileSuspended()
	lg.Spec.VPN.Size, lg.Spec.Gateway.Size = podSize(it.GetVpnSize()), podSize(it.GetGatewaySize())
	tenant := tenantOf(ctx)
	lg.Labels, lg.Annotations = dep.stamp(stampTenant(copyLabels(want), tenant), stampID(it.GetName()))
	reservation := ""
	if id := it.GetReservationId(); id != "" {
		reservation = crName(id)
		lg.Labels[names.LabelReservation] = reservation
	}

	_, err := groups.Create(ctx, lg, metav1.CreateOptions{})
	if err == nil {
		return result(ref, protobuf.ItemState_ITEM_STATE_CREATED)
	}
	if err = createErr(err, kindLabGroup, it.GetName(), func() (metav1.Object, error) {
		cur, err := groups.Get(ctx, name, metav1.GetOptions{})
		if err == nil && !ownedBy(tenant, cur) {
			// Another tenant's object: no terminating hint, the retry loop below answers.
			return nil, errTaken{kindLabGroup, it.GetName()}
		}
		return cur, err
	}); !apierrors.IsAlreadyExists(err) {
		return failedResult(ref, err)
	}

	state := protobuf.ItemState_ITEM_STATE_EXISTS
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		state = protobuf.ItemState_ITEM_STATE_EXISTS
		cur, err := groups.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !ownedBy(tenant, cur) {
			return errTaken{kindLabGroup, it.GetName()}
		}
		if err := rejectTerminating(kindLabGroup, cur); err != nil {
			return err
		}
		if names.IDOf(cur) != it.GetName() {
			return fmt.Errorf("%s %s: the name is taken by another id %q", kindLabGroup, it.GetName(), names.IDOf(cur))
		}
		if cur.Spec.Suspended != lg.Spec.Suspended || cur.VPNEnabled() != lg.VPNEnabled() || cur.InternetEnabled() != lg.InternetEnabled() ||
			cur.Spec.VPN.ProbeWhileSuspended != lg.Spec.VPN.ProbeWhileSuspended ||
			!samePodSize(cur.Spec.VPN.Size, lg.Spec.VPN.Size) || !samePodSize(cur.Spec.Gateway.Size, lg.Spec.Gateway.Size) || !dep.matches(cur.Labels, cur.Annotations) {
			return errDifferentSpec{kindLabGroup, it.GetName()}
		}
		if err := existingReservationLabel(kindLabGroup, it.GetName(), cur.Labels, reservation); err != nil {
			return err
		}
		add := want
		if reservation != "" {
			add = mergeItemLabels(want, map[string]string{names.LabelReservation: reservation})
		}
		labels, changed := mergeLabels(cur.Labels, add)
		if !changed {
			return nil
		}
		cur.Labels = labels
		state = protobuf.ItemState_ITEM_STATE_UPDATED
		_, err = groups.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return failedResult(ref, err)
	}
	return result(ref, state)
}

// errDifferentSpec is an existing object whose spec differs from the one sent.
type errDifferentSpec struct{ kind, name string }

// Reason is the machine code of the item (LAB_NAME_CONFLICT); the text stays as it was.
func (e errDifferentSpec) Reason() string {
	if e.kind == kindLab {
		return client.ReasonLabNameConflict
	}
	return ""
}

func (e errDifferentSpec) Error() string {
	return fmt.Sprintf("%s %s already exists with a different spec", e.kind, e.name)
}

// ListLabGroups lists LabGroups by names (items), by selector, or all of them.
func (h *Handler) ListLabGroups(ctx context.Context, in *protobuf.ListRequest) (*protobuf.LabGroupList, error) {
	if err := checkListRequest(in); err != nil {
		return nil, err
	}
	out := &protobuf.LabGroupList{}
	ids, err := h.reservationIDs(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, g := range out.Items {
			g.ReservationId = reservationIDOf(ids, g.GetReservationId())
		}
	}()
	if len(in.GetItems()) > 0 {
		for _, ref := range in.GetItems() {
			g, err := h.getGroup(ctx, ref.GetName())
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out.Items = append(out.Items, h.currentLabGroupProto(ctx, g))
		}
		return out, nil
	}
	items, err := h.listGroups(ctx, in.GetSelector())
	if err != nil {
		return nil, err
	}
	for i := range items {
		out.Items = append(out.Items, h.currentLabGroupProto(ctx, &items[i]))
	}
	return out, nil
}

// checkListRequest: items or a selector, never both; items must be non-empty refs.
func checkListRequest(in *protobuf.ListRequest) error {
	if in.GetSelector() != "" && len(in.GetItems()) > 0 {
		return invalid("give a selector or items, not both")
	}
	if err := parseSelector(in.GetSelector(), true); err != nil {
		return err
	}
	return checkItemCount(len(in.GetItems()))
}

type groupTarget struct {
	ref     *protobuf.ItemRef
	changes *protobuf.LabGroupChanges
}

// UpdateLabGroups changes labels and the mutable spec fields of the targeted groups.
func (h *Handler) UpdateLabGroups(ctx context.Context, in *protobuf.UpdateLabGroupsRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	var targets []groupTarget
	if in.GetBySelector() != nil {
		items, err := h.listGroups(ctx, in.GetBySelector().GetSelector())
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(items), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		for i := range items {
			targets = append(targets, groupTarget{ref: groupRef(names.IDOf(&items[i]))})
		}
		sort.Slice(targets, func(i, j int) bool { return refKey(targets[i].ref) < refKey(targets[j].ref) })
	} else {
		for _, it := range in.GetItems() {
			targets = append(targets, groupTarget{ref: groupRef(it.GetName()), changes: it.GetChanges()})
		}
	}
	plans := make([]groupPlan, len(targets))
	refs := make([]*protobuf.ItemRef, len(targets))
	for i, t := range targets {
		refs[i] = t.ref
		p, err := planGroupChanges(in.GetChanges(), t.changes)
		if err != nil {
			return nil, invalid("%s: %v", describeRef(t.ref), err)
		}
		plans[i] = p
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		return h.updateLabGroup(ctx, refs[i], plans[i])
	})}, nil
}

// groupPlan is a validated set of changes for one group.
type groupPlan struct {
	labels                                                 labelPlan
	suspended, vpnEnabled, internetEnabled, probeWhileSusp *bool
	// reservation: attach to this reservation id (see attachReservation)
	reservation *string
}

// planGroupChanges overlays the item's changes (nil in a selector call) on the
// request-level ones.
func planGroupChanges(common, item *protobuf.LabGroupChanges) (groupPlan, error) {
	var p groupPlan
	var err error
	if p.labels, err = mergeLabelChanges(common.GetLabels(), item.GetLabels()); err != nil {
		return p, err
	}
	pick := func(c, i *bool) *bool {
		if i != nil {
			return i
		}
		return c
	}
	if item == nil {
		item = &protobuf.LabGroupChanges{}
	}
	if common == nil {
		common = &protobuf.LabGroupChanges{}
	}
	p.suspended = pick(common.Suspended, item.Suspended)
	// the VPN switch: vpn_enabled, else the old vpn_disabled (true = off, false = on)
	vpnDisabled := pick(common.VpnDisabled, item.VpnDisabled)
	p.vpnEnabled = pick(common.VpnEnabled, item.VpnEnabled)
	if p.vpnEnabled == nil && vpnDisabled != nil {
		on := !*vpnDisabled
		p.vpnEnabled = &on
	} else if p.vpnEnabled != nil && vpnDisabled != nil && *vpnDisabled == *p.vpnEnabled {
		return p, fmt.Errorf("vpn_disabled and vpn_enabled say the opposite of each other")
	}
	p.internetEnabled = pick(common.InternetEnabled, item.InternetEnabled)
	p.probeWhileSusp = pick(common.ProbeWhileSuspended, item.ProbeWhileSuspended)
	if p.reservation = pickString(common.ReservationId, item.ReservationId); p.reservation != nil {
		if err := names.ValidateID(*p.reservation); err != nil {
			return p, fmt.Errorf("reservation_id: %w", err)
		}
	}
	return p, nil
}

// pickString is the item's value when it has one, else the common one.
func pickString(c, i *string) *string {
	if i != nil {
		return i
	}
	return c
}

func (h *Handler) updateLabGroup(ctx context.Context, ref *protobuf.ItemRef, p groupPlan) *protobuf.ItemResult {
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := groups.Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !ownedBy(tenantOf(ctx), cur) {
			return notFoundForeign(kindLabGroup, ref.GetName())
		}
		if err := rejectTerminating(kindLabGroup, cur); err != nil {
			return err
		}
		if cur.Annotations[names.AnnotationLifecycleRetirement] != "" {
			return fmt.Errorf("retired group cannot mutate configuration")
		}
		changed := false
		var c bool
		if cur.Labels, c = p.labels.apply(cur.Labels); c {
			changed = true
		}
		set := func(dst *bool, v *bool) {
			if v != nil && *dst != *v {
				*dst = *v
				changed = true
			}
		}
		set(&cur.Spec.Suspended, p.suspended)
		if c := h.cluster(); (p.vpnEnabled != nil && *p.vpnEnabled && !c.VPN) || (p.internetEnabled != nil && *p.internetEnabled && !c.Internet) {
			return h.switchOnRefusal(p)
		}
		if p.vpnEnabled != nil && cur.VPNEnabled() != *p.vpnEnabled {
			if !*p.vpnEnabled {
				if err := h.refuseServerInUse(ctx, cur, kindServerVPN); err != nil {
					return err
				}
			}
			on := *p.vpnEnabled
			cur.Spec.VPN.Enabled, cur.Spec.VPN.Disabled, changed = &on, !on, true
		}
		if p.internetEnabled != nil && cur.InternetEnabled() != *p.internetEnabled {
			if !*p.internetEnabled {
				if err := h.refuseServerInUse(ctx, cur, kindServerInternet); err != nil {
					return err
				}
			}
			on := *p.internetEnabled
			cur.Spec.Gateway.Enabled, changed = &on, true
		}
		set(&cur.Spec.VPN.ProbeWhileSuspended, p.probeWhileSusp)
		if p.reservation != nil {
			label, err := h.attachReservation(ctx, kindLabGroup, ref.GetName(), cur.Labels, *p.reservation)
			if err != nil {
				return err
			}
			if label != "" {
				// only the label: the spec, and so the pods, stay as they are
				cur.Labels = mergeItemLabels(cur.Labels, map[string]string{names.LabelReservation: label})
				changed = true
			}
		}
		if !changed {
			return nil
		}
		_, err = groups.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return failedResult(ref, err)
	}
	return result(ref, protobuf.ItemState_ITEM_STATE_UPDATED)
}

// DeleteLabGroups deletes LabGroups. Deletion is asynchronous (finalizers): it is
// finished once ListLabGroups no longer returns the group. Until then Create and the
// other writes on that name fail with a retryable TERMINATING error.
func (h *Handler) DeleteLabGroups(ctx context.Context, in *protobuf.DeleteRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	var refs []*protobuf.ItemRef
	if in.GetBySelector() != nil {
		items, err := h.listGroups(ctx, in.GetBySelector().GetSelector())
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(items), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		for i := range items {
			refs = append(refs, groupRef(names.IDOf(&items[i])))
		}
		sortRefs(refs)
	} else {
		for _, r := range in.GetItems() {
			refs = append(refs, groupRef(r.GetName()))
		}
		if err := dupRefs(refs); err != nil {
			return nil, err
		}
	}
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		g, err := h.getGroup(ctx, refs[i].GetName())
		if err != nil {
			return failedResult(refs[i], err)
		}
		for _, receipt := range g.Status.Creations {
			if !receipt.Committed {
				return failedResult(refs[i], fmt.Errorf("original birth admission is pending"))
			}
		}
		uid, rv := g.UID, g.ResourceVersion
		return deleteResult(refs[i], h.cs.LaboratoryV1alpha1().LabGroups().Delete(ctx, crName(refs[i].GetName()), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}))
	})}, nil
}

// deleteResult maps the error of a Delete call.
func deleteResult(ref *protobuf.ItemRef, err error) *protobuf.ItemResult {
	if err != nil {
		return failedResult(ref, err)
	}
	return result(ref, protobuf.ItemState_ITEM_STATE_DELETED)
}

// checkPodSizes refuses an explicit size of the group's VPN or gateway pod that is not positive in both values or is over the
// maximum this cluster reports (GetFeatures, group_pods). No size is fine: the pod gets the chart's default.
func (h *Handler) checkPodSizes(it *protobuf.LabGroupItem) error {
	g := h.features.GroupPods.Sizing
	for _, c := range []struct {
		what   string
		size   *protobuf.PodSize
		sizing grouppods.PodSizing
	}{{"vpn_size", it.GetVpnSize(), g.VPN}, {"gateway_size", it.GetGatewaySize(), g.Gateway}} {
		if c.size == nil {
			continue
		}
		if err := c.sizing.Check(c.size.GetCpuMillicores(), c.size.GetMemoryBytes()); err != nil {
			return fmt.Errorf("%s: %w", c.what, err)
		}
	}
	return nil
}

func podSize(s *protobuf.PodSize) *laboratoryv1alpha1.GroupPodSize {
	if s == nil {
		return nil
	}
	return &laboratoryv1alpha1.GroupPodSize{CPUMillicores: s.GetCpuMillicores(), MemoryBytes: s.GetMemoryBytes()}
}

func samePodSize(a, b *laboratoryv1alpha1.GroupPodSize) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// applyExpectedCounts turns the counts a group is made for into the sizes of its pods (formulas in code). An item that sets a count,
// or neither a count nor a size, is sized from the counts (the defaults fill what it leaves out); an item that sets only sizes keeps
// them, as before.
func (h *Handler) applyExpectedCounts(it *protobuf.LabGroupItem) (bool, error) {
	if h.features.GroupLimits.MaxUsers == 0 { // an agent without the group limits (not configured): the sizes as sent
		return false, nil
	}
	counts := it.GetExpectedUsers() != 0 || it.GetExpectedLabs() != 0
	if !counts && (it.GetVpnSize() != nil || it.GetGatewaySize() != nil) {
		return false, nil
	}
	users, labs, err := h.features.GroupLimits.Resolve(int(it.GetExpectedUsers()), int(it.GetExpectedLabs()))
	if err != nil {
		return false, err
	}
	limits := h.features.GroupLimits
	vpn, gw := grouppods.VPNSize(users, limits.VPNLegs(labs)), grouppods.GatewaySize(limits.InternetLegs(labs))
	it.VpnSize = &protobuf.PodSize{CpuMillicores: vpn.CPU, MemoryBytes: vpn.Memory}
	it.GatewaySize = &protobuf.PodSize{CpuMillicores: gw.CPU, MemoryBytes: gw.Memory}
	return true, nil
}

// Which server of a group a switch is about.
const (
	kindServerVPN      = "vpn"
	kindServerInternet = "internet"
)

// switchesOf is the switches of a new group from its item. An item that sets NEITHER vpn_enabled NOR internet_enabled is the old shape
// (a caller made before contract 2.7): the VPN follows vpn_disabled (the default is on) and the gateway is on, as groups always were. A
// caller that sets either field gets the explicit meaning: the one it leaves unset is OFF. Both are returned as pointers, to be written on
// the object as given.
func switchesOf(it *protobuf.LabGroupItem) (vpn, internet *bool) {
	var v, i bool
	if it.VpnEnabled == nil && it.InternetEnabled == nil {
		v, i = !it.GetVpnDisabled(), true
	} else {
		v, i = it.VpnEnabled != nil && *it.VpnEnabled, it.InternetEnabled != nil && *it.InternetEnabled
	}
	return &v, &i
}

// clusterSwitchesOf is switchesOf under what the laboratory serves: a server the laboratory does not run is off, and asking for it
// explicitly is refused (VPN_UNAVAILABLE, INTERNET_UNAVAILABLE); an item in the old shape just gets it off.
func (h *Handler) clusterSwitchesOf(it *protobuf.LabGroupItem) (vpn, internet *bool, err error) {
	vpn, internet = switchesOf(it)
	c := h.cluster()
	off := false
	if !c.VPN {
		if it.VpnEnabled != nil && *it.VpnEnabled {
			return nil, nil, refusal(client.ReasonVPNUnavailable, "this laboratory runs no VPN (vpn.enabled is off): the group's VPN cannot be turned on")
		}
		vpn = &off
	}
	if !c.Internet {
		if it.InternetEnabled != nil && *it.InternetEnabled {
			return nil, nil, refusal(client.ReasonInternetUnavailable, "this laboratory has no internet gateway (inetGateway.enabled is off): the group's internet cannot be turned on")
		}
		off2 := false
		internet = &off2
	}
	return vpn, internet, nil
}

// switchOnRefusal is the refusal of an update that turns on a server the laboratory does not run.
func (h *Handler) switchOnRefusal(p groupPlan) error {
	if p.vpnEnabled != nil && *p.vpnEnabled && !h.cluster().VPN {
		return refusal(client.ReasonVPNUnavailable, "this laboratory runs no VPN (vpn.enabled is off): the group's VPN cannot be turned on")
	}
	return refusal(client.ReasonInternetUnavailable, "this laboratory has no internet gateway (inetGateway.enabled is off): the group's internet cannot be turned on")
}

// groupSwitches validates the switches of an item and returns them.
func groupSwitches(it *protobuf.LabGroupItem) (vpn, internet bool, err error) {
	if it.VpnEnabled != nil && *it.VpnEnabled && it.GetVpnDisabled() {
		return false, false, fmt.Errorf("vpn_enabled and vpn_disabled say the opposite of each other")
	}
	v, i := switchesOf(it)
	return *v, *i, nil
}

// groupServerRefusal refuses a lab spec that needs a server its group has switched off: a VPN gateway needs the VPN, an Internet
// gateway needs the internet. The operator refuses the same for objects made by hand (validateGateways).
func groupServerRefusal(g *laboratoryv1alpha1.LabGroup, spec *laboratoryv1alpha1.LabSpec) error {
	if spec.HasVPN() && !g.VPNEnabled() {
		return refusal(client.ReasonGroupVPNOff, "the lab has a VPN gateway but the VPN of group %s is off: turn it on (vpn_enabled) first", names.IDOf(g))
	}
	if spec.HasInternet() && !g.InternetEnabled() {
		return refusal(client.ReasonGroupInternetOff, "the lab has an Internet gateway but the internet of group %s is off: turn it on (internet_enabled) first", names.IDOf(g))
	}
	return nil
}

// refuseServerInUse refuses to turn a server of the group off while a lab of the group has a gateway that needs it (a lab being
// deleted counts: its gateway is removed by the pod).
func (h *Handler) refuseServerInUse(ctx context.Context, g *laboratoryv1alpha1.LabGroup, server string) error {
	ns := laboratoryv1alpha1.LabGroupNamespaceOf(g)
	labs, err := h.cs.LaboratoryV1alpha1().Labs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	var using []string
	for i := range labs.Items {
		l := &labs.Items[i]
		if (server == kindServerVPN && l.Spec.HasVPN()) || (server == kindServerInternet && l.Spec.HasInternet()) {
			using = append(using, fmt.Sprintf("%q", names.IDOf(l)))
		}
	}
	if len(using) == 0 {
		return nil
	}
	sort.Strings(using)
	what := "VPN gateway"
	if server == kindServerInternet {
		what = "Internet gateway"
	}
	return refusal(client.ReasonGroupServerInUse, "the %s of group %s is used by lab(s) %s: delete them before turning it off", what, names.IDOf(g), strings.Join(using, ", "))
}
