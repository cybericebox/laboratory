package l7

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testPodIP is a pod address of the tests (a fixed documentation-style value, not random).
const testPodIP = "10.244.1.5"

// readyDevice is the Device of `device` in lab `labName` that the operator published: Ready, with a pod address.
func readyDevice(ns, labName, device, ip string) *lab.Device {
	return &lab.Device{
		ObjectMeta: metav1.ObjectMeta{Name: labName + "-" + device, Namespace: ns},
		Spec:       lab.DeviceSpec{Name: device, LabRef: labName},
		Status:     lab.DeviceStatus{Ready: ip != "", PodIP: ip, NodeName: "node-a"},
	}
}

func routeFixture(t *testing.T, objs ...client.Object) (*AccessReader, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	svc := exposedPortService("http", 8080)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{svc}, objs...)...).Build()
	return &AccessReader{Reader: c}, c
}

// The route is the pod address in the status of the Service's own device, on the Service's declared port; the Service has no ClusterIP
// (it is headless) and the name of the Service is nowhere in the address.
func TestRouteIsThePodAddressOfTheServicesOwnDevice(t *testing.T) {
	access, _ := routeFixture(t, readyDevice("legacy", "lab", "web", testPodIP))
	route, err := access.Route(context.Background(), "web-abc12w", "legacy")
	if err != nil || route.URL != "http://"+testPodIP+":8080" || route.Lab != "lab" {
		t.Fatalf("%+v %v", route, err)
	}
	if strings.Contains(route.URL, "web-abc12w") || strings.Contains(route.URL, "cluster.local") {
		t.Fatalf("no DNS name in the upstream: %s", route.URL)
	}
}

// An address that changes in the status (the pod was replaced) is the one used by the next request, with no restart and no cache of ours.
func TestRouteFollowsAChangeOfThePodAddress(t *testing.T) {
	access, c := routeFixture(t, readyDevice("legacy", "lab", "web", testPodIP))
	if r, err := access.Route(context.Background(), "web-abc12w", "legacy"); err != nil || r.URL != "http://10.244.1.5:8080" {
		t.Fatal(r, err)
	}
	d := &lab.Device{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "legacy", Name: "lab-web"}, d); err != nil {
		t.Fatal(err)
	}
	d.Status.PodIP = "10.244.7.9"
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if r, err := access.Route(context.Background(), "web-abc12w", "legacy"); err != nil || r.URL != "http://10.244.7.9:8080" {
		t.Fatalf("the new address is not used: %+v %v", r, err)
	}
	d.Status.Ready, d.Status.PodIP = false, ""
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Route(context.Background(), "web-abc12w", "legacy"); !errors.Is(err, ErrRouteNotReady) {
		t.Fatalf("a pod that is gone must not leave an address behind: %v", err)
	}
}

// Fail closed: no Ready flag, no address, or an address no pod has is "not ready", never a connection.
func TestRouteWithoutAUsableAddressIsNotReady(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
		ip    string
	}{
		{"not ready", false, testPodIP},
		{"no address", true, ""},
		{"garbage", true, "not-an-ip"},
		{"a name", true, "web-abc12w.legacy.svc.cluster.local"},
		{"loopback", true, "127.0.0.1"},
		{"loopback v6", true, "::1"},
		{"link local", true, "169.254.169.254"},
		{"unspecified", true, "0.0.0.0"},
		{"multicast", true, "224.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := readyDevice("legacy", "lab", "web", tc.ip)
			d.Status.Ready = tc.ready
			access, _ := routeFixture(t, d)
			if r, err := access.Route(context.Background(), "web-abc12w", "legacy"); !errors.Is(err, ErrRouteNotReady) {
				t.Fatalf("routed %+v: %v", r, err)
			}
		})
	}
}

// The status of a Device that is not the Service's own never steers the route: another lab, another device, another namespace, a deleting
// device. With no matching device there is no route at all (the answer for a host that does not exist).
func TestRouteNeverUsesTheStatusOfAnotherDevice(t *testing.T) {
	deleting := readyDevice("legacy", "lab", "web", testPodIP)
	now := metav1.Now()
	deleting.DeletionTimestamp, deleting.Finalizers = &now, []string{"held"}
	for name, objs := range map[string][]client.Object{
		"another lab":       {readyDevice("legacy", "other", "web", "10.244.9.9")},
		"another device":    {readyDevice("legacy", "lab", "db", "10.244.9.9")},
		"another namespace": {readyDevice("elsewhere", "lab", "web", "10.244.9.9")},
		"deleting":          {deleting},
		"none":              nil,
	} {
		t.Run(name, func(t *testing.T) {
			access, _ := routeFixture(t, objs...)
			r, err := access.Route(context.Background(), "web-abc12w", "legacy")
			if err == nil || errors.Is(err, ErrRouteNotReady) {
				t.Fatalf("routed %+v (%v)", r, err)
			}
		})
	}
}

// The compact cache keeps what the route needs and nothing of the pod's other status.
func TestCompactDeviceKeepsTheRouteStatus(t *testing.T) {
	d := readyDevice("legacy", "lab", "web", testPodIP)
	d.Status.PodName = "a-pod"
	out, err := CompactCacheObject(d)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*lab.Device)
	if got.Status.PodIP != testPodIP || !got.Status.Ready || got.Status.NodeName != "node-a" || got.Status.PodName != "" {
		t.Fatalf("%+v", got.Status)
	}
}

func TestCompactServiceKeepsNothingButThePorts(t *testing.T) {
	svc := exposedPortService("http", 8080)
	svc.Spec.ClusterIP = "10.96.0.7"
	out, err := CompactCacheObject(svc)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*corev1.Service).Spec; got.ClusterIP != "" || len(got.Ports) != 1 {
		t.Fatalf("%+v", got)
	}
}

// Through the real handler: the connection goes to the pod address and the device still sees the Host the client sent; a device that is not
// ready gets the starting page and no connection is made at all.
func TestHandlerConnectsToThePodAddressAndKeepsTheHost(t *testing.T) {
	var host string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { host = r.Host; _, _ = io.WriteString(w, "device") }))
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)
	dialed := []string{}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return (&net.Dialer{}).DialContext(ctx, network, backendURL.Host)
	}}
	defer transport.CloseIdleConnections()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
	member := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p-u1", Namespace: "legacy"}}
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "legacy"}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow}}}}
	dev := readyDevice("legacy", "lab", "web", testPodIP)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, member, policy, exposedPortService("http", 8080), dev, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "legacy"}}).Build()
	h, _, _, cookie := liveFixture(t, http.NotFoundHandler())
	h.WithAccessReader(&AccessReader{Reader: c})
	h.transport = transport
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://web-abc12w.challenges.example.com/", nil)
		req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(); rec.Code != 200 || rec.Body.String() != "device" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if want := net.JoinHostPort(testPodIP, "8080"); len(dialed) != 1 || dialed[0] != want {
		t.Fatalf("dialed %v, want %s", dialed, want)
	}
	if host != "web-abc12w.challenges.example.com" {
		t.Fatalf("the device must see the client's Host, got %q", host)
	}

	dev.Status.Ready, dev.Status.PodIP = false, ""
	if err := c.Update(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
	rec := get()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "4" {
		t.Fatalf("a device without an address must answer the starting page: %d %s", rec.Code, rec.Body.String())
	}
	if len(dialed) != 1 {
		t.Fatalf("no connection may be made without an address: %v", dialed)
	}
}
