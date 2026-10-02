package laboratory

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/cybericebox/laboratory/internal/admissioncheck"
	"github.com/cybericebox/laboratory/internal/names"
)

// The chart's ValidatingAdmissionPolicy objects, rendered by helm and applied to a real API server: the operator's
// ServiceAccount (impersonated, with cluster-admin RBAC so that only the admission policy limits it) may work in the
// namespaces of its LabGroups and nowhere else.
var _ = Describe("operator admission policy", Ordered, func() {
	const operator = "system:serviceaccount:laboratory-system:laboratory-controller-manager"
	var op client.Client

	BeforeAll(func() {
		if _, err := exec.LookPath("helm"); err != nil {
			Skip("helm is not installed")
		}
		out, err := exec.Command("helm", "template", "x", "../../../charts/laboratory", "--namespace", "laboratory-system", "--kube-version", "1.33.0",
			"--set", "operator.baseDomain=lab.example.com", "--set", "operator.publicVPNEndpoint=vpn.example.com:51820", "--set", "operator.supportEmail=a@example.com",
			"-s", "templates/operator/admission-policy.yaml").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		applied := 0
		for _, doc := range strings.Split(string(out), "\n---\n") {
			var obj unstructured.Unstructured
			if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil || obj.Object == nil {
				continue
			}
			Expect(k8sClient.Create(ctx, &obj)).To(Succeed(), doc)
			applied++
		}
		Expect(applied).To(Equal(6), "three policies and their bindings")

		// The classes the chart creates: the API server refuses a pod of a class that does not exist.
		for _, name := range []string{"laboratory-group", "laboratory-device", "laboratory-platform"} {
			Expect(k8sClient.Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Value: int32(100 + len(name))})).To(Succeed())
		}

		// Only the admission policy may stand between the operator and the cluster in this test.
		Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "adm-test-operator"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: operator}},
		})).To(Succeed())
		impersonating := rest.CopyConfig(cfg)
		impersonating.Impersonate = rest.ImpersonationConfig{UserName: operator}
		op, err = client.New(impersonating, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// The policies take a moment to be compiled and enforced.
		Eventually(func() error {
			err := op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"}})
			if err == nil {
				_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"}})
				return fmt.Errorf("created")
			}
			if !apierrors.IsForbidden(err) {
				return err
			}
			return nil
		}, 30*time.Second, 500*time.Millisecond).Should(Succeed(), "the policy must start denying")
	})

	// The shape of every pod the operator makes: drop ALL with a few additions, the runtime's seccomp profile.
	hardenedPod := func(name, ns string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PodSpec{
				PriorityClassName: "laboratory-device",
				SecurityContext:   &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{
					Name: "c", Image: "x",
					SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_ADMIN", "NET_RAW", "SYS_PTRACE", "CHOWN"}}},
				}},
			},
		}
	}
	groupNamespace := func(name string) {
		GinkgoHelper()
		Expect(op.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{names.LabelGroup: name}}})).To(Succeed())
	}
	denied := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), err.Error())
	}

	It("is verified by the operator at start: enforced for the operator, not for anyone else", func() {
		Expect(admissioncheck.Verify(ctx, op)).To(Succeed())
		// somebody the policies do not apply to is not confined: the check says so, which is what a cluster without them looks like
		err := admissioncheck.Verify(ctx, k8sClient)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not enforced"))
	})

	It("requires hostUsers=false on device pods when userNamespaces is on (the chart default)", func() {
		groupNamespace("adm-users")
		device := func(name string, hostUsers *bool) *corev1.Pod {
			p := hardenedPod(name, "adm-users")
			p.Labels = map[string]string{names.LabelDevice: "web"}
			p.Spec.HostUsers = hostUsers
			return p
		}
		no, yes := false, true
		denied(op.Create(ctx, device("dev-default", nil)))
		denied(op.Create(ctx, device("dev-host-users", &yes)))
		Expect(op.Create(ctx, device("dev-userns", &no))).To(Succeed())
		// the VPN and gateway pods are not devices: no user namespace needed
		Expect(op.Create(ctx, hardenedPod("vpn-like", "adm-users"))).To(Succeed())
	})

	It("lets the operator create the namespace of a LabGroup and write in it", func() {
		groupNamespace("adm-group")
		Expect(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "adm-group"}})).To(Succeed())
		Expect(op.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "adm-group"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "default", Namespace: "adm-group"}},
		})).To(Succeed())
		Expect(op.Create(ctx, hardenedPod("p", "adm-group"))).To(Succeed())
	})

	It("refuses a namespace that is not a LabGroup's, and any change of one that is not the operator's", func() {
		denied(op.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "adm-not-a-group"}}))
		var kube corev1.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kube-system"}, &kube)).To(Succeed())
		patch := client.MergeFrom(kube.DeepCopy())
		kube.Labels = map[string]string{"laboratory.cybericebox.com/group": "x"}
		denied(op.Patch(ctx, &kube, patch))
		denied(op.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
	})

	It("refuses writes outside the namespaces of its groups, and RoleBindings anywhere but there", func() {
		denied(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}))
		denied(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "kube-system"}}))
		// the release namespace is allowed for what the operator does there, but not for RoleBindings
		Expect(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adm-copy", Namespace: "laboratory-system"}})).To(Succeed())
		denied(op.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "laboratory-system"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "default", Namespace: "laboratory-system"}},
		}))
		// someone else is not limited by these policies
		Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adm-admin", Namespace: "default"}})).To(Succeed())
	})

	It("refuses pods that reach into the node", func() {
		groupNamespace("adm-pods")
		yes := true
		priv := &corev1.SecurityContext{Privileged: &yes}
		for name, spec := range map[string]corev1.PodSpec{
			"host-network": {HostNetwork: true, Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			"host-pid":     {HostPID: true, Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			"privileged":   {Containers: []corev1.Container{{Name: "c", Image: "x", SecurityContext: priv}}},
			"privileged-init": {InitContainers: []corev1.Container{{Name: "i", Image: "x", SecurityContext: priv}},
				Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			"host-path": {Containers: []corev1.Container{{Name: "c", Image: "x"}},
				Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}},
		} {
			denied(op.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "adm-pods"}, Spec: spec}))
		}
	})

	// The "never in any profile" list and the rest of what reaches the node, enforced on the pods the operator creates.
	It("allows the pods the operator makes and refuses the rest", func() {
		Expect(op.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "adm-shape", Labels: map[string]string{names.LabelGroup: "shape"}}})).To(Succeed())

		good := func(name string) *corev1.Pod {
			return &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "adm-shape"},
				Spec: corev1.PodSpec{
					PriorityClassName: "laboratory-group",
					SecurityContext:   &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{Name: "c", Image: "x", SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_ADMIN", "NET_RAW", "SYS_PTRACE", "CHOWN"}}}}},
				},
			}
		}
		caps := func(p *corev1.Pod, add ...corev1.Capability) {
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = add
		}
		Expect(op.Create(ctx, good("fine"))).To(Succeed(), "the pods the operator really makes are allowed")

		refused := map[string]func(*corev1.Pod){
			"SYS_ADMIN":           func(p *corev1.Pod) { caps(p, "NET_ADMIN", "SYS_ADMIN") },
			"SYS_MODULE":          func(p *corev1.Pod) { caps(p, "SYS_MODULE") },
			"DAC_READ_SEARCH":     func(p *corev1.Pod) { caps(p, "DAC_READ_SEARCH") },
			"no drop ALL":         func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Capabilities.Drop = nil },
			"no security context": func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext = nil },
			"init SYS_ADMIN": func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "i", Image: "x", SecurityContext: &corev1.SecurityContext{
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SYS_ADMIN"}}}}}
			},
			"seccomp unconfined": func(p *corev1.Pod) {
				p.Spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
			},
			"no seccomp": func(p *corev1.Pod) { p.Spec.SecurityContext = nil },
			"container unconfined": func(p *corev1.Pod) {
				p.Spec.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
			},
			"procMount unmasked": func(p *corev1.Pod) {
				no := false
				p.Spec.HostUsers = &no
				um := corev1.UnmaskedProcMount
				p.Spec.Containers[0].SecurityContext.ProcMount = &um
			},
			"host port": func(p *corev1.Pod) {
				p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 80, HostPort: 8080}}
			},
			"platform class":          func(p *corev1.Pod) { p.Spec.PriorityClassName = "laboratory-platform" },
			"system class":            func(p *corev1.Pod) { p.Spec.PriorityClassName = "system-node-critical" },
			"no class":                func(p *corev1.Pod) { p.Spec.PriorityClassName = "" },
			"node name":               func(p *corev1.Pod) { p.Spec.NodeName = "some-node" },
			"another service account": func(p *corev1.Pod) { p.Spec.ServiceAccountName = "laboratory-node-agent" },
			"nfs volume": func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{Name: "n", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "x", Path: "/"}}}}
			},
		}
		i := 0
		for name, mutate := range refused {
			i++
			p := good(fmt.Sprintf("bad-%d", i))
			mutate(p)
			err := op.Create(ctx, p)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%s must be refused, got %v", name, err)
		}
		for _, sa := range []string{"vpn", "gateway", "default"} {
			p := good("sa-" + sa)
			p.Spec.ServiceAccountName = sa
			p.Spec.Volumes = []corev1.Volume{{Name: "e", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			Expect(op.Create(ctx, p)).To(Succeed(), sa)
		}

		// A Deployment's template is judged the same way.
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "adm-shape"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}}, Spec: good("x").Spec},
			},
		}
		caps2 := &dep.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add
		*caps2 = []corev1.Capability{"SYS_ADMIN"}
		Expect(apierrors.IsForbidden(op.Create(ctx, dep))).To(BeTrue(), "a Deployment that adds SYS_ADMIN")
		*caps2 = []corev1.Capability{"NET_ADMIN"}
		Expect(op.Create(ctx, dep)).To(Succeed())

		// A pod that already runs may have its labels changed whatever it was made of (pods of earlier versions).
		old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "adm-shape"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}}}
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		patch := client.MergeFrom(old.DeepCopy())
		old.Labels = map[string]string{"team": "red"}
		Expect(op.Patch(ctx, old, patch)).To(Succeed())
	})
})
