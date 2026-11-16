package laboratory

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/gatewayspec"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/imagepolicy"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

// labSubnetPoolSize is the size of the lab-subnets pool for a lab base network: every /24 of the base but number 0, which belongs to the VPN
// clients. The pool starts at offset 1, so its indexes are the /24 numbers.
func labSubnetPoolSize(base string) (uint, error) {
	count, err := netutil.SubnetCount(base, labSubnetPrefixLen)
	if err != nil {
		return 0, err
	}
	if count < 2 {
		return 0, fmt.Errorf("lab base network %s has no room for a lab network", base)
	}
	return min(count-1, names.MaxLabNetworkIndex), nil
}

func (r *LabReconciler) maxLabGateways() int {
	if r.MaxLabGateways > 0 {
		return r.MaxLabGateways
	}
	return gatewayspec.DefaultMax
}

// validateGateways checks what the lab declares as gateways before anything is taken from the address pool for them (see package
// gatewayspec). The agent has refused the same at creation; a Lab made by hand or by an older agent is stopped here.
func (r *LabReconciler) validateGateways(lab *laboratoryv1alpha1.Lab) error {
	return gatewayspec.Validate(&lab.Spec, r.maxLabGateways())
}

// groupServerRecheck is how often a lab refused for a server its group has switched off is looked at again.
const groupServerRecheck = 15 * time.Second

// validateGatewaysAndServers is validateGateways followed by groupServerOff: the first refusal, and whether it is the one for a server
// of the group that is off (which is looked at again later).
func (r *LabReconciler) validateGatewaysAndServers(ctx context.Context, lab *laboratoryv1alpha1.Lab) (refusal error, serverOff bool, err error) {
	if refusal = r.validateGateways(lab); refusal != nil {
		return refusal, false, nil
	}
	// what the laboratory does not serve at all (the API server's admission policies and the agent refuse the same; this catches the rest)
	if r.Switches != nil {
		if rf := r.Switches.Refuse(&lab.Spec); rf != nil {
			return fmt.Errorf("%s: %s", rf.Reason, rf.Text), false, nil
		}
	}
	if refusal = r.validateRegistries(lab); refusal != nil {
		return refusal, false, nil
	}
	if refusal, err = r.tenantCredentialRefusal(ctx, lab); refusal != nil || err != nil {
		return refusal, false, err
	}
	off, err := r.groupServerOff(ctx, lab)
	return off, off != nil, err
}

// validateRegistries refuses a lab with a device image from a registry that is not on the list of allowed registries. An image of the
// platform registry or the cache (the node's localhost forwarder) is not looked at: the agent has admitted it, and its own rules apply.
func (r *LabReconciler) validateRegistries(lab *laboratoryv1alpha1.Lab) error {
	for i := range lab.Spec.Devices {
		img := lab.Spec.Devices[i].Image
		if img == "" || strings.HasPrefix(img, names.RegistryNodePrefix+"/") || imagepolicy.IsPlatformScheme(img) {
			continue
		}
		if err := imagepolicy.RegistryAllowed(r.AllowedRegistries, img); err != nil {
			return fmt.Errorf("%s: device %q: %v", imagepolicy.ReasonRegistryNotAllowed, lab.Spec.Devices[i].Name, err)
		}
		// With the image cache on a container image either goes through it or is refused: there is no direct pull.
		if lab.Spec.Devices[i].Type == laboratoryv1alpha1.DeviceTypeContainer {
			if _, err := r.Mirror.RewriteRequired(img); err != nil {
				return fmt.Errorf("%s: device %q: %v", imagepolicy.ReasonRegistryNotAllowed, lab.Spec.Devices[i].Name, err)
			}
		}
	}
	return nil
}

// groupServerOff is the refusal of a lab that declares a gateway whose server its group has switched off (spec.vpn.enabled for a VPN
// gateway, spec.gateway.enabled for an Internet gateway), nil when the lab is fine. A lab outside any group namespace is not checked. The
// agent refuses the same at creation (GROUP_VPN_OFF, GROUP_INTERNET_OFF); a Lab made by hand is stopped here, and it starts by itself
// when the switch is turned on.
func (r *LabReconciler) groupServerOff(ctx context.Context, lab *laboratoryv1alpha1.Lab) (refusal error, err error) {
	if !lab.Spec.HasVPN() && !lab.Spec.HasInternet() {
		return nil, nil
	}
	group, err := (&DeviceReconciler{Client: r.Client}).labGroupOfNamespace(ctx, lab.Namespace)
	if err != nil || group == nil {
		return nil, err
	}
	if lab.Spec.HasVPN() && !group.VPNEnabled() {
		return fmt.Errorf("GroupVPNOff: the lab has a VPN gateway but the VPN of its group %s is off (spec.vpn.enabled)", group.Name), nil
	}
	if lab.Spec.HasInternet() && !group.InternetEnabled() {
		return fmt.Errorf("GroupInternetOff: the lab has an Internet gateway but the internet of its group %s is off (spec.gateway.enabled)", group.Name), nil
	}
	return nil, nil
}

// setGatewayStatus writes the entry of a gateway into the status, in place (by name) or at the end.
func setGatewayStatus(status *laboratoryv1alpha1.LabStatus, entry laboratoryv1alpha1.GatewayStatus) {
	for i := range status.Gateways {
		if status.Gateways[i].Name == entry.Name {
			status.Gateways[i] = entry
			return
		}
	}
	status.Gateways = append(status.Gateways, entry)
}

// ensureSubnetAllocation gives every gateway of the lab its /24 (status.gateways): the first free number of the one lab-subnets pool of the
// group, for VPN and Internet gateways alike. A gateway that has its /24 keeps it. The number is recorded at once, one write per gateway,
// so a failure later does not leave a number taken from the pool unaccounted for.
func (r *LabReconciler) ensureSubnetAllocation(ctx context.Context, lab *laboratoryv1alpha1.Lab) (updated bool, err error) {
	for _, g := range lab.Spec.Gateways {
		if st := lab.Status.GatewayStatus(g.Name); st != nil && st.CIDR != "" {
			continue
		}
		size, err := labSubnetPoolSize(r.LabBaseNetwork)
		if err != nil {
			return updated, fmt.Errorf("size the lab subnet pool: %w", err)
		}
		allocator := &poolpkg.ForwardAllocator{Client: r.Client, Prefix: names.PoolLabSubnets, Namespace: lab.Namespace, Size: size, InUse: r.subnetsInUse(lab.Namespace)}
		n, err := allocator.AllocateIndex(ctx)
		if err != nil {
			return updated, fmt.Errorf("allocate lab subnet: %w", err)
		}
		cidr, err := netutil.SubnetForIndex(r.LabBaseNetwork, labSubnetPrefixLen, n)
		if err != nil {
			return updated, fmt.Errorf("compute the subnet of gateway %s: %w", g.Name, err)
		}
		setGatewayStatus(&lab.Status, laboratoryv1alpha1.GatewayStatus{Name: g.Name, Kind: g.Kind, CIDR: cidr, NetworkIndex: n})
		if err := r.Status().Update(ctx, lab); err != nil {
			return updated, err
		}
		updated = true
	}
	return updated, nil
}

// subnetsInUse is the numbers the labs of a namespace refer to, read straight from the API server (the insurance of the forward-only pool:
// see poolpkg.ForwardAllocator): the position in the base network of every /24 of a lab, and the number of every LabVPN and LabGateway
// leg.
func (r *LabReconciler) subnetsInUse(namespace string) func(context.Context) (map[uint]bool, error) {
	return func(ctx context.Context) (map[uint]bool, error) {
		reader := r.lifecycleReader()
		var labs laboratoryv1alpha1.LabList
		if err := reader.List(ctx, &labs, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		used := map[uint]bool{}
		mark := func(cidr string) {
			if cidr == "" {
				return
			}
			if n, err := netutil.SubnetIndex(r.LabBaseNetwork, labSubnetPrefixLen, cidr); err == nil {
				used[n] = true
			}
		}
		for i := range labs.Items {
			st := &labs.Items[i].Status
			for _, g := range st.Gateways {
				mark(g.CIDR)
				if g.NetworkIndex != 0 {
					used[g.NetworkIndex] = true
				}
			}
		}
		var vpns laboratoryv1alpha1.LabVPNList
		if err := reader.List(ctx, &vpns, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range vpns.Items {
			used[vpns.Items[i].Spec.NetworkIndex] = true
		}
		var gws laboratoryv1alpha1.LabGatewayList
		if err := reader.List(ctx, &gws, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range gws.Items {
			used[gws.Items[i].Spec.NetworkIndex] = true
		}
		return used, nil
	}
}

// ensureLabNetworkObjects creates the LabVPN or LabGateway of every gateway that has its /24 (and the DHCP pool of one that serves DHCP). The
// FinalizerController on each object signals the VPN and gateway programs that the spec is complete and they may start reconciling.
func (r *LabReconciler) ensureLabNetworkObjects(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	for _, g := range lab.Spec.Gateways {
		st := lab.Status.GatewayStatus(g.Name)
		if st == nil || st.CIDR == "" {
			continue
		}
		switch g.Kind {
		case laboratoryv1alpha1.GatewayKindVPN:
			if err := r.ensureLabVPN(ctx, lab, g, st.NetworkIndex); err != nil {
				return err
			}
		case laboratoryv1alpha1.GatewayKindInternet:
			if err := r.ensureLabGateway(ctx, lab, g, st.NetworkIndex); err != nil {
				return err
			}
		}
		if err := r.ensureDHCPPool(ctx, lab, g); err != nil {
			return err
		}
	}
	return nil
}

// ensureDHCPPool creates the DHCP Pool of a gateway that the VPN or gateway program uses as the enable-signal (and the future lease
// allocator) of its embedded DHCP server. Without this Pool the program silently never starts DHCP. The gateway's DHCPServer.Enabled gates
// creation. The owner reference ties the Pool's lifecycle to the Lab.
func (r *LabReconciler) ensureDHCPPool(ctx context.Context, lab *laboratoryv1alpha1.Lab, g laboratoryv1alpha1.GatewayTemplate) error {
	if g.DHCPServer == nil || !g.DHCPServer.Enabled {
		return nil
	}
	poolName, prefix := names.DHCPPoolName(lab.Name, g.Name), "dhcp-inet"
	if g.Kind == laboratoryv1alpha1.GatewayKindVPN {
		prefix = "dhcp-vpn"
	}
	var existing allocationv1alpha1.Pool
	if err := r.Get(ctx, types.NamespacedName{Name: poolName, Namespace: lab.Namespace}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Leases live in hosts .2..254 of the gateway /24; .1 is the gateway leg.
	const dhcpPoolOffset, dhcpPoolSize = 2, 253
	bitmapStr, free := poolpkg.InitBitmap(dhcpPoolSize, dhcpPoolOffset)
	p := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: lab.Namespace,
			Labels: map[string]string{
				poolpkg.PoolTypeLabel:   prefix,
				poolpkg.PoolStateLabel:  poolpkg.PoolStateEmpty,
				poolpkg.PoolGroupLabel:  names.DHCPPoolGroup(lab.Name, g.Name),
				poolpkg.LatestPoolLabel: valueTrue,
			},
		},
		Spec: allocationv1alpha1.PoolSpec{Size: dhcpPoolSize, Offset: dhcpPoolOffset},
	}
	if err := controllerutil.SetOwnerReference(lab, p, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, p); err != nil {
		return err
	}
	p.Status = allocationv1alpha1.PoolStatus{Free: free, BitMap: bitmapStr}
	return r.Status().Update(ctx, p)
}

func (r *LabReconciler) ensureLabVPN(ctx context.Context, lab *laboratoryv1alpha1.Lab, g laboratoryv1alpha1.GatewayTemplate, n uint) error {
	name := names.LabVPNObjectName(lab.Name, g.Name)
	var existing laboratoryv1alpha1.LabVPN
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	obj := &laboratoryv1alpha1.LabVPN{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  lab.Namespace,
			Finalizers: []string{names.FinalizerController},
		},
		Spec: laboratoryv1alpha1.LabVPNSpec{
			LabName:      lab.Name,
			Device:       g.Name,
			NetworkIndex: n,
		},
	}
	if err := controllerutil.SetOwnerReference(lab, obj, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, obj)
}

func (r *LabReconciler) ensureLabGateway(ctx context.Context, lab *laboratoryv1alpha1.Lab, g laboratoryv1alpha1.GatewayTemplate, n uint) error {
	name := names.LabGatewayObjectName(lab.Name, g.Name)
	var existing laboratoryv1alpha1.LabGateway
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	obj := &laboratoryv1alpha1.LabGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  lab.Namespace,
			Finalizers: []string{names.FinalizerController},
		},
		Spec: laboratoryv1alpha1.LabGatewaySpec{
			LabName:      lab.Name,
			Device:       g.Name,
			NetworkIndex: n,
		},
	}
	if err := controllerutil.SetOwnerReference(lab, obj, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, obj)
}

// ensureGatewaysDeleted triggers the deletion of every LabVPN and LabGateway object of the lab and, once the VPN or gateway program has
// removed its own finalizer (leaving only FinalizerController), removes FinalizerController so the object can be garbage-collected.
// It returns true when no such object exists any more.
func (r *LabReconciler) ensureGatewaysDeleted(ctx context.Context, lab *laboratoryv1alpha1.Lab) (bool, error) {
	done := true
	var vpns laboratoryv1alpha1.LabVPNList
	if err := r.List(ctx, &vpns, client.InNamespace(lab.Namespace)); err != nil {
		return false, err
	}
	for i := range vpns.Items {
		if vpns.Items[i].Spec.LabName != lab.Name {
			continue
		}
		ok, err := r.ensureGatewayObjectDeleted(ctx, &vpns.Items[i], names.FinalizerVPN, names.ComponentVPN)
		if err != nil {
			return false, err
		}
		done = done && ok
	}
	var gws laboratoryv1alpha1.LabGatewayList
	if err := r.List(ctx, &gws, client.InNamespace(lab.Namespace)); err != nil {
		return false, err
	}
	for i := range gws.Items {
		if gws.Items[i].Spec.LabName != lab.Name {
			continue
		}
		ok, err := r.ensureGatewayObjectDeleted(ctx, &gws.Items[i], names.FinalizerGateway, names.ComponentGateway)
		if err != nil {
			return false, err
		}
		done = done && ok
	}
	return done, nil
}

// ensureGatewayObjectDeleted deletes one LabVPN or LabGateway and waits for the program of the group's pod to clean up. The VPN pod is a
// Deployment, so if it crashes a new one comes up and processes the deletion; the only way it never runs is a group teardown, which
// LabGroup.reconcileDelete prevents by draining the Labs before the namespace (and the Deployment) is deleted.
func (r *LabReconciler) ensureGatewayObjectDeleted(ctx context.Context, obj client.Object, finalizer, component string) (bool, error) {
	if obj.GetDeletionTimestamp().IsZero() {
		return false, client.IgnoreNotFound(r.Delete(ctx, obj))
	}
	f := obj.GetFinalizers()
	if len(f) == 1 && f[0] == names.FinalizerController {
		controllerutil.RemoveFinalizer(obj, names.FinalizerController)
		return false, client.IgnoreNotFound(r.Update(ctx, obj))
	}
	return r.releaseFinalizerOfAbsentGroupPod(ctx, obj, finalizer, component)
}

// gatewayReadiness is the state of every gateway of the lab as the Lab reports it.
type gatewayReadiness struct {
	// Status is status.gateways.
	Status []laboratoryv1alpha1.GatewayStatus
	// AllReady is true when every gateway is ready (and true for a lab without gateways).
	AllReady bool
	// Dropped is the packets the block of connections started by the lab dropped, over the VPN gateways of the lab that confirmed the
	// block; Blocked says at least one did.
	Dropped int64
	Blocked bool
}

// readGatewayReadiness reads the LabVPN / LabGateway of every gateway of the lab. A gateway whose object is missing, or not Ready, is not
// ready.
func (r *LabReconciler) readGatewayReadiness(ctx context.Context, lab *laboratoryv1alpha1.Lab) (gatewayReadiness, error) {
	out := gatewayReadiness{AllReady: true}
	for _, g := range lab.Spec.Gateways {
		entry := laboratoryv1alpha1.GatewayStatus{Name: g.Name, Kind: g.Kind}
		if st := lab.Status.GatewayStatus(g.Name); st != nil {
			entry.CIDR, entry.NetworkIndex = st.CIDR, st.NetworkIndex
		}
		switch g.Kind {
		case laboratoryv1alpha1.GatewayKindVPN:
			leg := &laboratoryv1alpha1.LabVPN{}
			ready, err := r.segmentReady(ctx, lab.Namespace, names.LabVPNObjectName(lab.Name, g.Name), leg)
			if err != nil {
				return out, err
			}
			entry.Ready = ready
			// The block of connections started by the lab is reported only as the gateway of the group confirmed it on the LabVPN: a
			// group whose VPN pod still runs an older program never confirms, and the lab is then not reported as blocked.
			entry.LabInitiatedBlocked = labInitiatedBlockedStatus(g, leg)
			if entry.LabInitiatedBlocked {
				out.Blocked = true
				out.Dropped += leg.Status.LabInitiatedDropped
			}
		case laboratoryv1alpha1.GatewayKindInternet:
			ready, err := r.segmentReady(ctx, lab.Namespace, names.LabGatewayObjectName(lab.Name, g.Name), &laboratoryv1alpha1.LabGateway{})
			if err != nil {
				return out, err
			}
			entry.Ready = ready
		}
		if !entry.Ready {
			out.AllReady = false
		}
		out.Status = append(out.Status, entry)
	}
	return out, nil
}

// segmentReady reports whether the LabVPN / LabGateway object is Ready. A missing object is not ready yet.
func (r *LabReconciler) segmentReady(ctx context.Context, ns, name string, obj client.Object) (bool, error) {
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	switch o := obj.(type) {
	case *laboratoryv1alpha1.LabVPN:
		return labstatus.IsReady(o.Status.Conditions), nil
	case *laboratoryv1alpha1.LabGateway:
		return labstatus.IsReady(o.Status.Conditions), nil
	}
	return false, nil
}

// gatewayEndpoint says whether a connection endpoint names a gateway of the lab, and which. A gateway has one port, eth0.
func gatewayEndpoint(lab *laboratoryv1alpha1.Lab, ep laboratoryv1alpha1.EndpointSpec) (laboratoryv1alpha1.GatewayTemplate, bool) {
	return lab.Spec.Gateway(ep.Device)
}

// tenantCredentialRefusal refuses a lab of a tenant whose images.pullSecret holds an entry without a username while the image cache is on:
// the cache carries the tenant in the username, so such a credential could never pull, and the pods would fail later with a message that
// does not say why. The Tenant says the same in its condition ImageCredentials; the lab starts by itself once the Secret is fixed.
func (r *LabReconciler) tenantCredentialRefusal(ctx context.Context, lab *laboratoryv1alpha1.Lab) (refusal error, err error) {
	if !r.Mirror.Enabled() {
		return nil, nil
	}
	secret, err := tenantPullSecret(ctx, r, names.TenantOf(lab.Labels))
	if err != nil || secret == "" {
		return nil, err
	}
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: names.TenantsNamespace, Name: secret}, &s); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	bad, _ := imagecache.WithoutUsername(s.Data[corev1.DockerConfigJsonKey])
	if len(bad) == 0 {
		return nil, nil
	}
	return stderrors.New(CredentialRefusalMessage(bad)), nil
}
