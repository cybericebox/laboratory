package laboratory

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/cybericebox/laboratory/internal/names"
)

// C-18: the node-agent may patch the Node object of its own node, and change only the node-agent-ready label of it to "true".
var _ = Describe("node-agent label admission policy", Ordered, func() {
	const nodeAgent = "system:serviceaccount:laboratory-system:laboratory-node-agent"
	var na client.Client

	patchLabels := func(c client.Client, node string, labels map[string]any) error {
		return c.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}},
			client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"metadata":{"labels":%s}}`, toJSON(labels)))))
	}

	BeforeAll(func() {
		if _, err := exec.LookPath("helm"); err != nil {
			Skip("helm is not installed")
		}
		out, err := exec.Command("helm", "template", "x", "../../../charts/laboratory", "--namespace", "laboratory-system", "--kube-version", "1.33.0",
			"--set", "operator.baseDomain=lab.example.com", "--set", "operator.publicVPNEndpoint=vpn.example.com:51820", "--set", "operator.supportEmail=a@example.com",
			"-s", "templates/node-agent/admission-policy.yaml").CombinedOutput()
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
		Expect(applied).To(Equal(4), "the Node-ready and capture policies and their bindings")

		Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "adm-test-node-agent"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: nodeAgent}},
		})).To(Succeed())
		for _, n := range []string{"na-node1", "na-node2"} {
			Expect(k8sClient.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: map[string]string{"pool": "labs"}}})).To(Succeed())
		}
		impersonating := rest.CopyConfig(cfg)
		impersonating.Impersonate = rest.ImpersonationConfig{UserName: nodeAgent, Extra: map[string][]string{"authentication.kubernetes.io/node-name": {"na-node1"}}}
		na, err = client.New(impersonating, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// The policy takes a moment to be compiled and enforced.
		Eventually(func() error {
			err := na.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "na-node1"}},
				client.RawPatch(types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`)))
			if err == nil {
				_ = k8sClient.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "na-node1"}}, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"unschedulable":false}}`)))
				return fmt.Errorf("changed the spec")
			}
			if !apierrors.IsForbidden(err) {
				return err
			}
			return nil
		}, 30*time.Second, 500*time.Millisecond).Should(Succeed(), "the policy must start denying")
	})

	It("sets and removes the ready label of its own node", func() {
		Expect(patchLabels(na, "na-node1", map[string]any{names.LabelNodeAgentReady: "true"})).To(Succeed())
		var n corev1.Node
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "na-node1"}, &n)).To(Succeed())
		Expect(n.Labels).To(HaveKeyWithValue(names.LabelNodeAgentReady, "true"))
		Expect(n.Labels).To(HaveKeyWithValue("pool", "labs"))
		Expect(patchLabels(na, "na-node1", map[string]any{names.LabelNodeAgentReady: nil})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "na-node1"}, &n)).To(Succeed())
		Expect(n.Labels).NotTo(HaveKey(names.LabelNodeAgentReady))
	})

	It("refuses everything else", func() {
		forbidden := func(err error, what string) {
			ExpectWithOffset(1, apierrors.IsForbidden(err)).To(BeTrue(), what+": %v", err)
		}
		forbidden(patchLabels(na, "na-node2", map[string]any{names.LabelNodeAgentReady: "true"}), "another node")
		forbidden(patchLabels(na, "na-node1", map[string]any{names.LabelNodeAgentReady: "false"}), "another value")
		forbidden(patchLabels(na, "na-node1", map[string]any{"pool": "other"}), "changing another label")
		forbidden(patchLabels(na, "na-node1", map[string]any{"pool": nil}), "removing another label")
		forbidden(patchLabels(na, "na-node1", map[string]any{"laboratory.cybericebox.com/other": "x"}), "adding another label")
		forbidden(na.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "na-node1"}},
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"taints":[{"key":"x","effect":"NoExecute"}]}}`))), "a taint")
		forbidden(na.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "na-node1"}},
			client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"a":"b"}}}`))), "an annotation")
	})
})

func toJSON(v map[string]any) string {
	var parts []string
	for k, val := range v {
		if val == nil {
			parts = append(parts, fmt.Sprintf("%q:null", k))
		} else {
			parts = append(parts, fmt.Sprintf("%q:%q", k, val))
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}
