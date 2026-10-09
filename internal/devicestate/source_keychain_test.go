package devicestate

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
	"time"
)

type podSourceTestRuntime struct {
	*fakeRuntime
	calls       int
	legacyCalls int
	pod         PodInfo
	container   Container
	err         error
}

func (r *podSourceTestRuntime) LoadImageForPod(_ context.Context, c Container, p PodInfo) (v1.Image, error) {
	r.calls++
	r.pod = p
	r.container = c
	return nil, r.err
}
func (r *podSourceTestRuntime) LoadImage(ctx context.Context, ref string) (v1.Image, error) {
	r.legacyCalls++
	return r.fakeRuntime.LoadImage(ctx, ref)
}

func TestEnginePodSourceRuntimePreservesOwnedContextAndErrors(t *testing.T) {
	for _, path := range []string{"snapshot", "publish-start", "required-empty-restored"} {
		t.Run(path, func(t *testing.T) {
			r := newRig(t, time.Hour, 1<<20)
			ctx := context.Background()
			if path != "snapshot" {
				base := r.rt.images["docker.io/library/app:1"]
				h, err := base.Digest()
				if err != nil {
					t.Fatal(err)
				}
				ref := r.reg.Host + "/lab/ns/lab/web@" + h.String()
				r.rt.images[ref] = base
				r.rt.imageRef = ref
			}
			failure := errors.New("pod-owned credential lookup failed")
			rt := &podSourceTestRuntime{fakeRuntime: r.rt, err: failure}
			r.e.Runtime = rt
			tr := r.track(t)
			defer r.e.stopAll()
			var err error
			switch path {
			case "snapshot":
				r.rt.setDiff(tarOf(map[string]string{"data/value": "state"}))
				err = tr.snapshot(ctx, true)
			case "publish-start":
				err = tr.publishStart(ctx)
			case "required-empty-restored":
				r.rt.setDiff(tarOf(map[string]string{}))
				err = tr.snapshotLocked(ctx, false, true)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("%s must propagate pod-owned source error, got %v", path, err)
			}
			if rt.calls != 1 || rt.legacyCalls != 0 || rt.container.ID != tr.c.ID || rt.pod.Device != tr.pod.Device || rt.pod.Pod != tr.pod.Pod || rt.pod.ContainerID != tr.pod.ContainerID {
				t.Fatalf("lost ownership context or fell back to unscoped runtime: calls=%d legacy=%d container=%s pod=%s", rt.calls, rt.legacyCalls, rt.container.ID, rt.pod.Pod)
			}
			records, _, _ := r.cl.snapshot()
			if len(records) != 0 {
				t.Fatal("failed pod-owned source lookup must not publish a snapshot")
			}
		})
	}
}

func podSourceFixture(t *testing.T) (client.Client, Container, PodInfo) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "device-pod", UID: types.UID("pod-a"), Annotations: map[string]string{names.AnnotationStateDevice: "device-a", names.AnnotationStateEpoch: "0", names.AnnotationStateIncarnation: "1"}}, Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "web", Image: "registry.example/source/app:current"}}, ImagePullSecrets: []corev1.LocalObjectReference{{Name: "source-pull"}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "web", Image: "registry.example/source/app:current", ContainerID: "containerd://container-a"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "source-pull"}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"registry.example":{"username":"pod-reader","password":"read-only"},"foreign.example":{"username":"must-not-send","password":"foreign-password"}}}`)}}
	foreign := secret.DeepCopy()
	foreign.Namespace = "tenant-b"
	foreign.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{"registry.example":{"username":"foreign-tenant","password":"must-not-read"}}}`)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, secret, foreign).WithStatusSubresource(&corev1.Pod{}).Build(), Container{ID: "container-a", ImageRef: "registry.example/source/app@sha256:" + strings.Repeat("a", 64)}, PodInfo{Device: types.NamespacedName{Namespace: "tenant-a", Name: "device-a"}, Pod: "device-pod", UID: "pod-a", ContainerID: "container-a", Incarnation: 1}
}

func sourceAuth(t *testing.T, keychain authn.Keychain, repository string) authn.AuthConfig {
	t.Helper()
	repo, err := name.NewRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	a, err := keychain.Resolve(repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	return *cfg
}

func TestPodSourceKeychainUsesOnlyFreshPodNamespacePullSecrets(t *testing.T) {
	reader, c, p := podSourceFixture(t)
	get := NewPodSourceKeychain(reader, "node-a")
	kc, err := get(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := sourceAuth(t, kc, "registry.example/source/app"); cfg.Username != "pod-reader" || cfg.Password != "read-only" {
		t.Fatal("did not use pod-owned read credentials")
	}
	if cfg := sourceAuth(t, kc, "foreign.example/source/app"); cfg.Username != "" || cfg.Password != "" {
		t.Fatal("sent credentials to foreign host")
	}
	if cfg := sourceAuth(t, kc, "registry.example/other/app"); cfg.Username != "" || cfg.Password != "" {
		t.Fatal("sent source credentials to another repository")
	}
	var secret corev1.Secret
	if err := reader.Get(context.Background(), types.NamespacedName{Namespace: p.Device.Namespace, Name: "source-pull"}, &secret); err != nil {
		t.Fatal(err)
	}
	secret.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{"registry.example":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("rotated-reader:rotated-password")) + `"}}}`)
	if err := reader.Update(context.Background(), &secret); err != nil {
		t.Fatal(err)
	}
	kc, err = get(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := sourceAuth(t, kc, "registry.example/source/app"); cfg.Username != "rotated-reader" || cfg.Password != "rotated-password" {
		t.Fatal("source credential rotation was cached")
	}
}

func TestPodSourceKeychainAnonymousWithoutPullSecrets(t *testing.T) {
	reader, c, p := podSourceFixture(t)
	var pod corev1.Pod
	key := types.NamespacedName{Namespace: p.Device.Namespace, Name: p.Pod}
	if err := reader.Get(context.Background(), key, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.ImagePullSecrets = nil
	if err := reader.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	kc, err := NewPodSourceKeychain(reader, "node-a")(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := sourceAuth(t, kc, "registry.example/source/app"); cfg.Username != "" || cfg.Password != "" {
		t.Fatal("anonymous pod unexpectedly acquired credentials")
	}
}

func TestPodSourceKeychainRejectsStaleOwnershipAndImage(t *testing.T) {
	for _, which := range []string{"uid", "node", "device", "epoch", "incarnation", "missing-epoch", "malformed-incarnation", "container-id", "pod-image", "status-image", "missing-status", "device-epoch"} {
		t.Run(which, func(t *testing.T) {
			reader, c, p := podSourceFixture(t)
			var pod corev1.Pod
			key := types.NamespacedName{Namespace: p.Device.Namespace, Name: p.Pod}
			if err := reader.Get(context.Background(), key, &pod); err != nil {
				t.Fatal(err)
			}
			switch which {
			case "uid":
				p.UID = "replacement"
			case "node":
				pod.Spec.NodeName = "other-node"
			case "device":
				pod.Annotations[names.AnnotationStateDevice] = "other-device"
			case "epoch":
				pod.Annotations[names.AnnotationStateEpoch] = "1"
			case "incarnation":
				pod.Annotations[names.AnnotationStateIncarnation] = "2"
			case "missing-epoch":
				delete(pod.Annotations, names.AnnotationStateEpoch)
			case "malformed-incarnation":
				pod.Annotations[names.AnnotationStateIncarnation] = "invalid"
			case "container-id":
				p.ContainerID = "other-container"
			case "pod-image":
				pod.Spec.Containers[0].Image = "foreign.example/source/app:current"
			case "status-image":
				pod.Status.ContainerStatuses[0].Image = "registry.example/foreign/app:current"
			case "missing-status":
				pod.Status.ContainerStatuses = nil
			case "device-epoch":
				p.DeviceEpoch = 1
			}
			status := pod.Status.DeepCopy()
			if err := reader.Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			pod.Status = *status
			if err := reader.Status().Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			if kc, err := NewPodSourceKeychain(reader, "node-a")(context.Background(), c, p); err == nil || kc != nil {
				t.Fatal("stale or foreign pod acquired source credentials")
			}
		})
	}
}

func TestPodSourceKeychainRejectsMissingMalformedAndForeignSecrets(t *testing.T) {
	for _, which := range []string{"missing", "foreign-only", "wrong-type", "missing-data", "malformed-json", "missing-auths", "malformed-auth"} {
		t.Run(which, func(t *testing.T) {
			reader, c, p := podSourceFixture(t)
			var secret corev1.Secret
			key := types.NamespacedName{Namespace: p.Device.Namespace, Name: "source-pull"}
			if err := reader.Get(context.Background(), key, &secret); err != nil {
				t.Fatal(err)
			}
			switch which {
			case "missing", "foreign-only":
				if err := reader.Delete(context.Background(), &secret); err != nil {
					t.Fatal(err)
				}
			case "wrong-type":
				secret.Type = corev1.SecretTypeOpaque
			case "missing-data":
				secret.Data = nil
			case "malformed-json":
				secret.Data[corev1.DockerConfigJsonKey] = []byte(`{invalid`)
			case "missing-auths":
				secret.Data[corev1.DockerConfigJsonKey] = []byte(`{}`)
			case "malformed-auth":
				secret.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{"registry.example":{"auth":"invalid-base64!"}}}`)
			}
			if which != "missing" && which != "foreign-only" {
				if err := reader.Update(context.Background(), &secret); err != nil {
					t.Fatal(err)
				}
			}
			if kc, err := NewPodSourceKeychain(reader, "node-a")(context.Background(), c, p); err == nil || kc != nil {
				t.Fatal("unusable pod pull Secret became anonymous or foreign credentials")
			}
		})
	}
}

type sourceReaderFailure struct {
	client.Reader
	secretOnly bool
	failure    error
}

func (r sourceReaderFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if !r.secretOnly || key.Name == "source-pull" {
		return r.failure
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
func TestPodSourceKeychainPreservesPodAndSecretAPIErrors(t *testing.T) {
	for _, secretOnly := range []bool{false, true} {
		reader, c, p := podSourceFixture(t)
		failure := errors.New("source API unavailable")
		r := sourceReaderFailure{Reader: reader, secretOnly: secretOnly, failure: failure}
		if kc, err := NewPodSourceKeychain(r, "node-a")(context.Background(), c, p); !errors.Is(err, failure) || kc != nil {
			t.Fatal("API error became anonymous credentials")
		}
	}
}
