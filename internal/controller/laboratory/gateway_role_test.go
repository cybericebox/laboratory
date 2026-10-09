package laboratory

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func TestGatewayRoleReadsLabsInItsNamespace(t *testing.T) {
	if err := lab.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("helm", "template", "laboratory", filepath.Join("..", "..", "..", "charts", "laboratory"), "--namespace", "laboratory-system", "-s", "templates/operator/clusterrole-gateway.yaml", "--set", "operator.baseDomain=lab.example.com", "--set", "operator.supportEmail=support@example.com", "--set", "operator.publicVPNEndpoint=vpn.example.com:51820").CombinedOutput()
	if err != nil {
		t.Fatalf("render role: %v %s", err, out)
	}
	var role rbac.ClusterRole
	if err := yaml.Unmarshal(out, &role); err != nil {
		t.Fatal(err)
	}
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir()}
	e.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"gateway-own", "gateway-other"} {
		if err := admin.Create(ctx, &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := admin.Create(ctx, &role); err != nil {
		t.Fatal(err)
	}
	controller := &LabGroupReconciler{Client: admin}
	if err := controller.ensureRoleBinding(ctx, "gateway-own", names.ComponentGateway, names.RoleGatewayName); err != nil {
		t.Fatal(err)
	}
	userCfg := rest.CopyConfig(cfg)
	userCfg.Impersonate.UserName = "system:serviceaccount:gateway-own:" + names.ComponentGateway
	user, err := client.NewWithWatch(userCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	var labs lab.LabList
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = user.List(ctx, &labs, client.InNamespace("gateway-own"))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	watch, err := user.Watch(ctx, &lab.LabList{}, client.InNamespace("gateway-own"))
	if err != nil {
		t.Fatal(err)
	}
	watch.Stop()
	if err := user.Get(ctx, client.ObjectKey{Namespace: "gateway-own", Name: "missing"}, &lab.Lab{}); !apierrors.IsNotFound(err) {
		t.Fatalf("get expected authorized NotFound, got %v", err)
	}
	if err := user.List(ctx, &labs, client.InNamespace("gateway-other")); !apierrors.IsForbidden(err) {
		t.Fatalf("cross namespace access %v", err)
	}
	if err := user.Create(ctx, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "forbidden-write", Namespace: "gateway-own"}}); !apierrors.IsForbidden(err) {
		t.Fatalf("unexpected Labs write %v", err)
	}
}
