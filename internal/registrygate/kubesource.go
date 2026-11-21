package registrygate

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// KubeReviewer is the TokenReviewer over the API server: a TokenReview with the audiences the gate accepts.
type KubeReviewer struct{ Client kubernetes.Interface }

// Review implements TokenReviewer.
func (k KubeReviewer) Review(ctx context.Context, token string, audiences []string) (Review, error) {
	tr, err := k.Client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return Review{}, err
	}
	extra := map[string][]string{}
	for k, v := range tr.Status.User.Extra {
		extra[k] = []string(v)
	}
	return Review{Authenticated: tr.Status.Authenticated, Username: tr.Status.User.Username, Audiences: tr.Status.Audiences, Extra: extra, Error: tr.Status.Error}, nil
}

// KubeSources keeps the Schedule and the SecretView up to date from the API server by watches (a cache of controller-runtime), and says
// at every step that the contact with the API server is alive: a successful watch event, and a heartbeat call every HeartbeatEvery.
type KubeSources struct {
	Config *rest.Config
	// SecretNamespace is the namespace of the upstream credentials (names.UpstreamNamespace).
	SecretNamespace string
	Schedule        *Schedule
	Secrets         *SecretView
	// HeartbeatEvery is how often the API server is asked for its version to prove that it is reachable (default 20 seconds).
	HeartbeatEvery time.Duration
	// OnError is told about a heartbeat that failed.
	OnError func(error)

	health informerHealth
	infs   map[string]interface{ LastSyncResourceVersion() string }
}

// Clientset is the typed client the token reviews and the heartbeat use.
func (k *KubeSources) Clientset() (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(k.Config)
}

// Run starts the watches and the heartbeat and blocks until ctx ends. The Schedule is marked in contact as soon as the caches have synced.
func (k *KubeSources) Run(ctx context.Context) error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	cs, err := k.Clientset()
	if err != nil {
		return err
	}
	ns := k.SecretNamespace
	if ns == "" {
		ns = names.UpstreamNamespace
	}
	c, err := cache.New(k.Config, cache.Options{
		Scheme: scheme,
		DefaultWatchErrorHandler: func(ctx context.Context, r *toolscache.Reflector, err error) {
			k.health.WatchError(r.TypeDescription())
			toolscache.DefaultWatchErrorHandler(ctx, r, err)
		},
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Namespaces: map[string]cache.Config{ns: {}}},
			&corev1.Pod{}:    {Transform: trimPod},
			&corev1.Node{}:   {Transform: trimNode},
		},
	})
	if err != nil {
		return err
	}
	if err := k.addHandlers(ctx, c); err != nil {
		return err
	}
	go func() { _ = c.Start(ctx) }()
	if !c.WaitForCacheSync(ctx) {
		return fmt.Errorf("the caches did not sync")
	}
	lister, err := client.New(k.Config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	k.Schedule.Confirm()
	every := k.HeartbeatEvery
	if every <= 0 {
		every = 20 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := k.beat(ctx, cs, lister, ns); err != nil {
				if k.OnError != nil {
					k.OnError(err)
				}
				continue
			}
			k.Schedule.Confirm()
		}
	}
}

// beat is the proof of life: the API server answers, a cheap list of every kind the view is made of succeeds, and no watch of those kinds
// has failed lately. Only then is the view "in contact". An event of one kind proves nothing about the others, so events do not confirm.
func (k *KubeSources) beat(ctx context.Context, cs kubernetes.Interface, c client.Client, secretNS string) error {
	if _, err := cs.Discovery().ServerVersion(); err != nil {
		return err
	}
	lists := map[string]client.ObjectList{
		"Pod": &corev1.PodList{}, "Node": &corev1.NodeList{}, "Device": &laboratoryv1alpha1.DeviceList{},
		"LabGroup": &laboratoryv1alpha1.LabGroupList{}, "ImagePull": &laboratoryv1alpha1.ImagePullList{},
		"Tenant": &laboratoryv1alpha1.TenantList{}, "Secret": &corev1.SecretList{},
	}
	for kind, l := range lists {
		opts := []client.ListOption{client.Limit(1)}
		if kind == "Secret" {
			opts = append(opts, client.InNamespace(secretNS))
		}
		err := c.List(ctx, l, opts...)
		k.health.ListResult(kind, err)
		if err == nil {
			if acc, aerr := meta.ListAccessor(l); aerr == nil {
				if v := k.infs[kind]; v != nil {
					k.health.Observe(kind, v.LastSyncResourceVersion(), acc.GetResourceVersion())
				}
			}
		}
	}
	if ok, kind := k.health.Healthy(); !ok {
		return fmt.Errorf("the view of %s is not healthy", kind)
	}
	return nil
}

// kindName is the name a kind goes by in the health table.
func kindName(obj client.Object) string {
	switch obj.(type) {
	case *corev1.Pod:
		return "Pod"
	case *corev1.Node:
		return "Node"
	case *laboratoryv1alpha1.Device:
		return "Device"
	case *laboratoryv1alpha1.LabGroup:
		return "LabGroup"
	case *laboratoryv1alpha1.ImagePull:
		return "ImagePull"
	case *laboratoryv1alpha1.Tenant:
		return "Tenant"
	case *corev1.Secret:
		return "Secret"
	}
	return ""
}

func (k *KubeSources) addHandlers(ctx context.Context, c cache.Cache) error {
	add := func(obj client.Object, set func(any), del func(any)) error {
		inf, err := c.GetInformer(ctx, obj)
		if err != nil {
			return err
		}
		if v, ok := inf.(interface{ LastSyncResourceVersion() string }); ok {
			if k.infs == nil {
				k.infs = map[string]interface{ LastSyncResourceVersion() string }{}
			}
			k.infs[kindName(obj)] = v
		}
		_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { set(o) },
			UpdateFunc: func(_, o any) { set(o) },
			DeleteFunc: func(o any) {
				if d, ok := o.(toolscache.DeletedFinalStateUnknown); ok {
					o = d.Obj
				}
				del(o)
			},
		})
		return err
	}
	s := k.Schedule
	if err := add(&corev1.Pod{}, func(o any) { s.SetPod(podInfo(o.(*corev1.Pod))) }, func(o any) {
		p := o.(*corev1.Pod)
		s.DeletePod(p.Namespace, p.Name)
	}); err != nil {
		return err
	}
	if err := add(&corev1.Node{}, func(o any) { s.SetNode(nodeInfo(o.(*corev1.Node))) }, func(o any) { s.DeleteNode(o.(*corev1.Node).Name) }); err != nil {
		return err
	}
	if err := add(&laboratoryv1alpha1.Device{}, func(o any) { s.SetDevice(deviceInfo(o.(*laboratoryv1alpha1.Device))) }, func(o any) {
		d := o.(*laboratoryv1alpha1.Device)
		s.DeleteDevice(d.Namespace, d.Spec.LabRef, d.Spec.Name)
	}); err != nil {
		return err
	}
	if err := add(&laboratoryv1alpha1.LabGroup{}, func(o any) {
		g := o.(*laboratoryv1alpha1.LabGroup)
		s.SetGroup(laboratoryv1alpha1.LabGroupNamespaceOf(g), names.TenantOf(g.Labels))
	}, func(o any) { s.DeleteGroup(laboratoryv1alpha1.LabGroupNamespaceOf(o.(*laboratoryv1alpha1.LabGroup))) }); err != nil {
		return err
	}
	if err := add(&laboratoryv1alpha1.ImagePull{}, func(o any) { s.SetImagePull(imagePullInfo(o.(*laboratoryv1alpha1.ImagePull))) }, func(o any) {
		s.DeleteImagePull(o.(*laboratoryv1alpha1.ImagePull).Name)
	}); err != nil {
		return err
	}
	if err := add(&laboratoryv1alpha1.Tenant{}, func(o any) { s.SetTenant(tenantInfo(o.(*laboratoryv1alpha1.Tenant))) }, func(o any) { s.DeleteTenant(o.(*laboratoryv1alpha1.Tenant).Name) }); err != nil {
		return err
	}
	return add(&corev1.Secret{}, func(o any) {
		sec := o.(*corev1.Secret)
		if sec.Type == corev1.SecretTypeDockerConfigJson {
			k.Secrets.Set(sec.Name, sec.Data[corev1.DockerConfigJsonKey])
		}
	}, func(o any) { k.Secrets.Delete(o.(*corev1.Secret).Name) })
}

// trimPod keeps only what the gate reads of a pod, so that the cache of every pod of the cluster stays small.
func trimPod(o any) (any, error) {
	p, ok := o.(*corev1.Pod)
	if !ok {
		return o, nil
	}
	out := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, UID: p.UID, Labels: p.Labels, ResourceVersion: p.ResourceVersion, DeletionTimestamp: p.DeletionTimestamp},
		Spec:       corev1.PodSpec{NodeName: p.Spec.NodeName},
		Status:     corev1.PodStatus{Phase: p.Status.Phase},
	}
	for _, c := range p.Spec.InitContainers {
		out.Spec.InitContainers = append(out.Spec.InitContainers, corev1.Container{Name: c.Name, Image: c.Image})
	}
	for _, c := range p.Spec.Containers {
		out.Spec.Containers = append(out.Spec.Containers, corev1.Container{Name: c.Name, Image: c.Image})
	}
	return out, nil
}

func trimNode(o any) (any, error) {
	n, ok := o.(*corev1.Node)
	if !ok {
		return o, nil
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.Name, ResourceVersion: n.ResourceVersion}, Status: corev1.NodeStatus{Addresses: n.Status.Addresses}}, nil
}

func podInfo(p *corev1.Pod) PodInfo {
	info := PodInfo{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID), Node: p.Spec.NodeName, Labels: p.Labels, Phase: string(p.Status.Phase), Terminating: p.DeletionTimestamp != nil}
	for _, c := range p.Spec.InitContainers {
		info.Images = append(info.Images, c.Image)
	}
	for _, c := range p.Spec.Containers {
		info.Images = append(info.Images, c.Image)
	}
	return info
}

func nodeInfo(n *corev1.Node) NodeInfo {
	out := NodeInfo{Name: n.Name}
	for _, a := range n.Status.Addresses {
		if a.Type != corev1.NodeInternalIP && a.Type != corev1.NodeExternalIP {
			continue
		}
		if ip, err := netip.ParseAddr(a.Address); err == nil {
			out.Addrs = append(out.Addrs, ip)
		}
	}
	return out
}

func deviceInfo(d *laboratoryv1alpha1.Device) DeviceInfo {
	info := DeviceInfo{Namespace: d.Namespace, Lab: d.Spec.LabRef, Name: d.Spec.Name, Node: d.Status.NodeName, StateEnabled: d.Spec.StateEnabled(), Tenant: names.TenantOf(d.Labels)}
	if d.Spec.State != nil {
		info.TenantQuotaBytes = d.Spec.State.TenantQuotaBytes
		info.WriteQuotaBytes = d.Spec.State.WriteQuotaBytes
	}
	return info
}

func imagePullInfo(p *laboratoryv1alpha1.ImagePull) ImagePullInfo {
	return ImagePullInfo{Name: p.Name, Nodes: p.Spec.Nodes, Images: p.Spec.Images, Tenant: p.Spec.Tenant}
}

func tenantInfo(t *laboratoryv1alpha1.Tenant) TenantInfo {
	info := TenantInfo{Name: t.Name}
	if q := t.Spec.Persistence.RegistryQuota; q != "" {
		if v, err := resource.ParseQuantity(q); err == nil {
			info.RegistryQuotaBytes = v.Value()
		}
	}
	return info
}
