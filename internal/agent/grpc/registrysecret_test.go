package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func regCred(registry, username, password string) *protobuf.RegistryCredential {
	return &protobuf.RegistryCredential{Registry: registry, Username: username, Password: password}
}

func TestDockerConfigJSON(t *testing.T) {
	raw, err := dockerConfigJSON([]*protobuf.RegistryCredential{regCred("index.docker.io", "u", "p")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"auths":{"index.docker.io":{"username":"u","password":"p","auth":"dTpw"}}}`; string(raw) != want {
		t.Fatalf("got %s, want %s", raw, want)
	}
}

func TestValidateRegistryCredentials(t *testing.T) {
	ok := []*protobuf.RegistryCredential{regCred("index.docker.io", "u", "p"), regCred("registry.local:5000", "", "tok")}
	if err := validateRegistryCredentials(ok); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := validateRegistryCredentials(nil); err != nil {
		t.Fatalf("none: %v", err)
	}
	tooMany := make([]*protobuf.RegistryCredential, 9)
	for i := range tooMany {
		tooMany[i] = regCred("r"+string(rune('a'+i))+".io", "u", "p")
	}
	for name, creds := range map[string][]*protobuf.RegistryCredential{
		"empty registry": {regCred("", "u", "secret-pw")},
		"empty password": {regCred("ghcr.io", "u", "")},
		"scheme":         {regCred("https://x", "u", "secret-pw")},
		"path":           {regCred("x/y", "u", "secret-pw")},
		"duplicate":      {regCred("ghcr.io", "u", "secret-pw"), regCred("GHCR.io", "v", "secret-pw")},
		"colon in user":  {regCred("ghcr.io", "a:b", "secret-pw")},
		"nil entry":      {nil},
		"more than 8":    tooMany,
	} {
		err := validateRegistryCredentials(creds)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if strings.Contains(err.Error(), "secret-pw") {
			t.Errorf("%s: the error shows the password: %v", name, err)
		}
	}
}

// registrySecret reads the per-lab registry Secret (nil when absent).
func registrySecret(ctx context.Context, t *testing.T, h *Handler, ns, lab string) *corev1.Secret {
	t.Helper()
	s, err := h.k8s.CoreV1().Secrets(ns).Get(ctx, names.LabRegistrySecret(lab), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func specWithPullSecret(t *testing.T, secret string) []byte {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(specJSON("web"), &spec); err != nil {
		t.Fatal(err)
	}
	spec["imagePullSecret"] = secret
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCreateLabWritesRegistrySecret(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-lab", "team-lab", nil)
	res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{
			{VariantId: "private", SpecJson: specWithPullSecret(t, "someone-else"), RegistryCredentials: []*protobuf.RegistryCredential{regCred("index.docker.io", "u", "p")}},
			{VariantId: "public", SpecJson: specWithPullSecret(t, "someone-else")},
		},
		Items: []*protobuf.LabItem{
			{LabGroup: "team-lab", Name: "l1", VariantId: "private"},
			{LabGroup: "team-lab", Name: "l2", VariantId: "public"},
		},
	})
	wantStates(t, res, err, stCreated, stCreated)

	l1, err := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(ctx, "l1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l1.Spec.ImagePullSecret != names.LabRegistrySecret("l1") {
		t.Fatalf("l1 imagePullSecret = %q", l1.Spec.ImagePullSecret)
	}
	s := registrySecret(ctx, t, h, "team-lab", "l1")
	if s == nil {
		t.Fatal("no registry Secret for l1")
	}
	if s.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("type %q", s.Type)
	}
	if want := `{"auths":{"index.docker.io":{"username":"u","password":"p","auth":"dTpw"}}}`; string(s.Data[corev1.DockerConfigJsonKey]) != want {
		t.Fatalf("data %s", s.Data[corev1.DockerConfigJsonKey])
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "Lab" || s.OwnerReferences[0].UID != l1.UID {
		t.Fatalf("owner %+v", s.OwnerReferences)
	}

	l2, err := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(ctx, "l2", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l2.Spec.ImagePullSecret != "" {
		t.Fatalf("l2 imagePullSecret = %q", l2.Spec.ImagePullSecret)
	}
	if registrySecret(ctx, t, h, "team-lab", "l2") != nil {
		t.Fatal("a lab without credentials has no registry Secret")
	}

	// Invalid credentials refuse the whole call.
	_, err = h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "bad", SpecJson: specJSON("web"), RegistryCredentials: []*protobuf.RegistryCredential{regCred("https://x", "u", "p")}}},
		Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "l3", VariantId: "bad"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid credentials: %v", err)
	}
}

// A resend of an existing lab (the daemon redeploys with fresh credentials) replaces the
// Secret: delete then create, never read or updated.
func TestUpdateLabReplacesRegistrySecret(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-lab", "team-lab", nil)
	send := func(password string) {
		t.Helper()
		res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web"), RegistryCredentials: []*protobuf.RegistryCredential{regCred("ghcr.io", "u", password)}}},
			Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "l1", VariantId: "v"}},
		})
		if err != nil || len(res.GetResults()) != 1 || res.Results[0].State == stFailed {
			t.Fatalf("create: %v %v", res, err)
		}
	}
	send("old")
	first := registrySecret(ctx, t, h, "team-lab", "l1")
	if first == nil {
		t.Fatal("no Secret")
	}
	send("new")
	second := registrySecret(ctx, t, h, "team-lab", "l1")
	if second == nil {
		t.Fatal("no Secret after the resend")
	}
	if second.UID == first.UID {
		t.Fatal("the Secret must be recreated, not updated")
	}
	var cfg struct {
		Auths map[string]struct{ Password string } `json:"auths"`
	}
	if err := json.Unmarshal(second.Data[corev1.DockerConfigJsonKey], &cfg); err != nil || cfg.Auths["ghcr.io"].Password != "new" {
		t.Fatalf("new credentials: %v", err)
	}
	// Removed behind the agent's back: a resend brings it back.
	if err := k8s.CoreV1().Secrets("team-lab").Delete(ctx, names.LabRegistrySecret("l1"), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	send("new")
	if registrySecret(ctx, t, h, "team-lab", "l1") == nil {
		t.Fatal("the resend must restore the Secret")
	}
	lab, err := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(ctx, "l1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lab.Spec.ImagePullSecret != names.LabRegistrySecret("l1") {
		t.Fatalf("imagePullSecret = %q", lab.Spec.ImagePullSecret)
	}
}

// A lifecycle-managed lab answers a resend before the device Secrets are written, but its
// registry Secret is still refreshed: a rotated token must reach a lab that is stopped and
// started later. The device env Secrets stay as they are.
func TestLifecycleLabResendRefreshesRegistrySecret(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-lab", "team-lab", nil)
	send := func(password string) {
		t.Helper()
		res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web"), RegistryCredentials: []*protobuf.RegistryCredential{regCred("ghcr.io", "u", password)}}},
			Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "l1", VariantId: "v"}},
		})
		if err != nil || len(res.GetResults()) != 1 || res.Results[0].State == stFailed {
			t.Fatalf("create: %v %v", res, err)
		}
	}
	send("old")
	labs := h.cs.LaboratoryV1alpha1().Labs("team-lab")
	lab, err := labs.Get(ctx, "l1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lab.Spec.Lifecycle = &laboratoryv1alpha1.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1, SnapshotMode: "Skip"}
	if _, err := labs.Update(ctx, lab, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	send("new")
	s := registrySecret(ctx, t, h, "team-lab", "l1")
	if s == nil {
		t.Fatal("no registry Secret after the resend")
	}
	var cfg struct {
		Auths map[string]struct{ Password string } `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil || cfg.Auths["ghcr.io"].Password != "new" {
		t.Fatalf("a lifecycle lab keeps the old credentials: %v", err)
	}
}

// Two agent replicas resending the same lab race between the delete and the create: the
// loser's create finds the winner's Secret. It replaces it once more instead of failing.
func TestWriteRegistrySecretRetriesAlreadyExists(t *testing.T) {
	k := k8sfake.NewClientset()
	creates := 0
	k.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		creates++
		if creates == 1 {
			return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), names.LabRegistrySecret("l1"))
		}
		return false, nil, nil
	})
	h := NewHandler(nil, k, nil)
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: "ns", UID: "u1"}}
	if err := h.writeRegistrySecret(context.Background(), lab, []*protobuf.RegistryCredential{regCred("ghcr.io", "u", "p")}); err != nil {
		t.Fatalf("a concurrent create must be retried: %v", err)
	}
	if creates != 2 {
		t.Fatalf("creates = %d, want 2", creates)
	}
	s, err := k.CoreV1().Secrets("ns").Get(context.Background(), names.LabRegistrySecret("l1"), metav1.GetOptions{})
	if err != nil || s.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("secret %v %v", s, err)
	}

	// A second conflict in a row is reported.
	k2 := k8sfake.NewClientset()
	k2.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), names.LabRegistrySecret("l1"))
	})
	if err := NewHandler(nil, k2, nil).writeRegistrySecret(context.Background(), lab, []*protobuf.RegistryCredential{regCred("ghcr.io", "u", "p")}); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("persistent conflict: %v", err)
	}
}
