package l7

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func exposedPortService(protocol string, port int32) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "legacy", Labels: map[string]string{names.LabelLab: "lab", names.LabelDevice: "web"}}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: []corev1.ServicePort{{Name: protocol, Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP}}}}
}

func TestAccessDeclaredPortRealHandlerDestination(t *testing.T) {
	for _, tc := range []struct {
		scheme string
		port   int32
	}{{"http", 8080}, {"https", 8443}} {
		t.Run(tc.scheme, func(t *testing.T) {
			response := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "owned endpoint") })
			var backend *httptest.Server
			if tc.scheme == "https" {
				backend = httptest.NewTLSServer(response)
			} else {
				backend = httptest.NewServer(response)
			}
			defer backend.Close()
			backendURL, _ := url.Parse(backend.URL)
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = lab.AddToScheme(scheme)
			group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Status: lab.LabGroupStatus{Namespace: "legacy"}}
			member := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p-u1", Namespace: "legacy"}}
			policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "legacy"}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow}}}}
			svc := exposedPortService(tc.scheme, tc.port)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, member, policy, svc, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "legacy"}}).Build()
			reader := &accessCountingReader{Reader: c, gets: map[string]int{}}
			h, _, _, cookie := liveFixture(t, http.NotFoundHandler())
			h.WithAccessReader(&AccessReader{Reader: reader})
			want := fmt.Sprintf("web-abc123.legacy.svc.cluster.local:%d", tc.port)
			transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != want {
					return nil, fmt.Errorf("handler selected %s instead of declared %s", address, want)
				}
				return (&net.Dialer{}).DialContext(ctx, network, backendURL.Host)
			}}
			defer transport.CloseIdleConnections()
			h.transport = transport
			req := httptest.NewRequest(http.MethodGet, "http://web-abc123.challenges.example.com/", nil)
			req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != "owned endpoint" || reader.gets[svc.Name] != 1 {
				t.Fatalf("real handler missed declared headless endpoint: status=%d body=%s service_reads=%d", rec.Code, rec.Body.String(), reader.gets[svc.Name])
			}
		})
	}
}

func TestAccessDeclaredPortCompactDefaultsAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		name, protocol  string
		port            int32
		transport       corev1.Protocol
		deleting, valid bool
	}{
		{"http default", "http", 80, corev1.ProtocolTCP, false, true},
		{"https default", "https", 443, corev1.ProtocolTCP, false, true},
		{"empty scheme default", "", 8080, "", false, true},
		{"nondefault http", "http", 8080, corev1.ProtocolTCP, false, true},
		{"nondefault https", "https", 8443, corev1.ProtocolTCP, false, true},
		{"zero", "http", 0, corev1.ProtocolTCP, false, false},
		{"negative", "http", -1, corev1.ProtocolTCP, false, false},
		{"range", "http", 65536, corev1.ProtocolTCP, false, false},
		{"unknown scheme", "ftp", 8080, corev1.ProtocolTCP, false, false},
		{"udp", "http", 8080, corev1.ProtocolUDP, false, false},
		{"deleting", "http", 8080, corev1.ProtocolTCP, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := exposedPortService(tc.protocol, tc.port)
			svc.Spec.Ports[0].Protocol = tc.transport
			if tc.deleting {
				now := metav1.Now()
				svc.DeletionTimestamp = &now
				svc.Finalizers = []string{"held"}
			}
			compact, err := CompactCacheObject(svc)
			if err != nil {
				t.Fatal(err)
			}
			copied := compact.(*corev1.Service)
			if copied.Spec.Ports[0].Port != tc.port || copied.Spec.Ports[0].Protocol != tc.transport {
				t.Fatal("compact cache discarded routing port")
			}
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(copied).Build()
			route, err := (&AccessReader{Reader: c}).Route(context.Background(), svc.Name, svc.Namespace)
			if tc.valid {
				protocol := tc.protocol
				if protocol == "" {
					protocol = "http"
				}
				want := fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", protocol, svc.Name, svc.Namespace, tc.port)
				if err != nil || route.URL != want {
					t.Fatalf("declared compact route: %v %v want=%s", route, err, want)
				}
			} else if err == nil {
				t.Fatalf("invalid/deleting Service routed: %+v", route)
			}
			copied.Spec.Ports[0].Port = 1234
			if svc.Spec.Ports[0].Port != tc.port {
				t.Fatal("compact transform changed original Service")
			}
		})
	}
}
