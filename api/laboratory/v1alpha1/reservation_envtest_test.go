package v1alpha1

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestReservationValidation(t *testing.T) {
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		dirs, _ := filepath.Glob("../../../bin/k8s/*")
		if len(dirs) == 0 {
			t.Fatal("no envtest assets: set KUBEBUILDER_ASSETS or run make setup-envtest")
		}
		dir = dirs[len(dirs)-1]
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: dir,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	if err := AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	from := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	mk := func(name, tenant string, to time.Time, res corev1.ResourceList) *Reservation {
		return &Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: ReservationSpec{
				Tenant:    tenant,
				From:      metav1.NewTime(from),
				To:        metav1.NewTime(to),
				Resources: res,
			},
		}
	}
	ok := corev1.ResourceList{"cpu": resource.MustParse("4"), "memory": resource.MustParse("8Gi")}

	h := from.Add(time.Hour)
	for name, tc := range map[string]struct {
		r    *Reservation
		want string
	}{
		"to-equals-from": {mk("a", "t1", from, ok), "spec.to must be after spec.from"},
		"to-before-from": {mk("b", "t1", from.Add(-time.Hour), ok), "spec.to must be after spec.from"},
		"gpu":            {mk("c", "t1", h, corev1.ResourceList{"gpu": resource.MustParse("1")}), "resources names only cpu and memory"},
		"bad-tenant":     {mk("d", "Not_A_Label", h, ok), "should match"},
		"long-tenant":    {mk("e", strings.Repeat("a", 64), h, ok), "Too long"},
		"long-name":      {mk(strings.Repeat("n", 64), "t1", h, ok), "at most 63 characters"},
	} {
		err := c.Create(ctx, tc.r)
		if err == nil {
			t.Errorf("%s: invalid Reservation accepted", name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not contain %q", name, err, tc.want)
		}
	}

	good := mk("good", "t1", from.Add(time.Hour), ok)
	if err := c.Create(ctx, good); err != nil {
		t.Fatalf("valid Reservation rejected: %v", err)
	}

	// `kubectl get res` asks for a Table: the columns must be Tenant, From, To, Phase.
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(cfg.Host, "/")+"/apis/laboratory.cybericebox.com/v1alpha1/reservations", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var table metav1.Table
	if err := json.Unmarshal(body, &table); err != nil {
		t.Fatalf("%v: %s (%s)", err, body, resp.Request.URL)
	}
	var cols []string
	for _, col := range table.ColumnDefinitions {
		cols = append(cols, col.Name)
	}
	if got := strings.Join(cols, ","); got != "Name,Tenant,From,To,Phase" {
		t.Errorf("columns = %s", got)
	}
	if len(table.Rows) != 1 || table.Rows[0].Cells[1] != "t1" {
		t.Errorf("rows = %+v", table.Rows)
	}
}
