package chart_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// operatorGrants renders every Role and ClusterRole the operator is bound to (its cluster role, the namespaced role that is
// bound in each group namespace and in the release namespace, the leader-election role, the tenants and images roles) and returns
// the set "group|resource|verb" they grant.
func operatorGrants(t *testing.T) map[string]bool {
	t.Helper()
	out, err := helmTemplate(t, "-s", "templates/operator/clusterrole.yaml", "-s", "templates/operator/clusterrole-namespaced.yaml",
		"-s", "templates/operator/role.yaml", "-s", "templates/tenants-namespace.yaml", "-s", "templates/images-namespace.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	granted := map[string]bool{}
	for _, d := range docs(t, out) {
		if k := d["kind"]; k != "Role" && k != "ClusterRole" {
			continue
		}
		for _, r := range rulesOf(t, d) {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, v := range r.Verbs {
						granted[g+"|"+res+"|"+v] = true
					}
				}
			}
		}
	}
	return granted
}

var markerRE = regexp.MustCompile(`^//\s*\+kubebuilder:rbac:groups=([^,]*),resources=([^,]*),verbs=(\S+)`)

// Every rule of the kubebuilder markers in the controllers (what the code says it needs) is granted by the chart's roles.
func TestChartRolesCoverTheRBACMarkers(t *testing.T) {
	granted := operatorGrants(t)
	files, _ := filepath.Glob("../../internal/controller/laboratory/*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(raw), "\n") {
			m := markerRE.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			group := strings.Trim(m[1], `"`)
			for _, res := range strings.Split(m[2], ";") {
				for _, verb := range strings.Split(m[3], ";") {
					if !granted[group+"|"+res+"|"+verb] {
						t.Errorf("%s: the marker wants %s %s/%s, no chart role grants it", filepath.Base(f), verb, group, res)
					}
				}
			}
		}
	}
}

// kinds maps the Go type of an object the operator writes to its API group and resource.
var kinds = map[string][2]string{
	"Pod": {"", "pods"}, "Secret": {"", "secrets"}, "Service": {"", "services"}, "ServiceAccount": {"", "serviceaccounts"},
	"Namespace": {"", "namespaces"}, "ConfigMap": {"", "configmaps"}, "Endpoints": {"", "endpoints"},
	"Deployment": {"apps", "deployments"}, "DaemonSet": {"apps", "daemonsets"},
	"NetworkPolicy": {"networking.k8s.io", "networkpolicies"}, "PodDisruptionBudget": {"policy", "poddisruptionbudgets"},
	"RoleBinding": {"rbac.authorization.k8s.io", "rolebindings"},
	"Lab":         {"laboratory.cybericebox.com", "labs"}, "LabGroup": {"laboratory.cybericebox.com", "labgroups"},
	"Device": {"laboratory.cybericebox.com", "devices"}, "Connection": {"laboratory.cybericebox.com", "connections"},
	"LabGroupClient": {"laboratory.cybericebox.com", "labgroupclients"}, "Tenant": {"laboratory.cybericebox.com", "tenants"},
	"ImagePull": {"laboratory.cybericebox.com", "imagepulls"}, "LabVPN": {"laboratory.cybericebox.com", "labvpns"},
	"LabGateway": {"laboratory.cybericebox.com", "labgateways"}, "Pool": {"allocation.cybericebox.com", "pools"},
}

// typeOf names the Go type a write argument has, where it can be told from the declarations of the function: `&pkg.T{...}`,
// a variable declared `var x pkg.T` or `x := &pkg.T{...}`, or an element of a `pkg.TList`.
func typeOf(e ast.Expr, vars map[string]string) string {
	switch x := e.(type) {
	case *ast.UnaryExpr:
		return typeOf(x.X, vars)
	case *ast.CompositeLit:
		return selName(x.Type)
	case *ast.Ident:
		return vars[x.Name]
	case *ast.IndexExpr: // pods.Items[i]
		if sel, ok := x.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Items" {
			if id, ok := sel.X.(*ast.Ident); ok {
				return strings.TrimSuffix(vars[id.Name], "List")
			}
		}
	}
	return ""
}

func selName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.StarExpr:
		return selName(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// Every write the operator's controllers make on an object whose type is known is granted by the chart's roles. The patch of
// the user labels of pods (regression of the RBAC rework) is the case this was written for.
func TestChartRolesCoverTheVerbsTheCodeUses(t *testing.T) {
	granted := operatorGrants(t)
	fset := token.NewFileSet()
	files, _ := filepath.Glob("../../internal/controller/laboratory/*.go")
	used := map[string][]string{} // "group|resource|verb" -> where
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			vars := map[string]string{}
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.ValueSpec: // var pod corev1.Pod
					for _, name := range x.Names {
						if x.Type != nil {
							vars[name.Name] = selName(x.Type)
						}
					}
				case *ast.AssignStmt: // p := &corev1.Pod{...}  /  p := &pods.Items[i]
					if x.Tok == token.DEFINE && len(x.Lhs) == len(x.Rhs) {
						for i, l := range x.Lhs {
							if id, ok := l.(*ast.Ident); ok {
								if ty := typeOf(x.Rhs[i], vars); ty != "" {
									vars[id.Name] = ty
								}
							}
						}
					}
				}
				return true
			})
			// range over a list's Items: for i := range pods.Items { p := &pods.Items[i] } is covered by the IndexExpr rule
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "CreateOrUpdate" && len(call.Args) >= 3 { // controllerutil.CreateOrUpdate(ctx, c, obj, mutate)
					if k, known := kinds[typeOf(call.Args[2], vars)]; known {
						for _, v := range []string{"create", "update"} {
							used[k[0]+"|"+k[1]+"|"+v] = append(used[k[0]+"|"+k[1]+"|"+v], filepath.Base(f))
						}
					}
					return true
				}
				verb := map[string]string{"Create": "create", "Update": "update", "Patch": "patch", "Delete": "delete"}[sel.Sel.Name]
				if verb == "" || len(call.Args) < 2 {
					return true
				}
				suffix := ""
				if inner, ok := sel.X.(*ast.CallExpr); ok { // r.Status().Update
					if is, ok := inner.Fun.(*ast.SelectorExpr); ok && is.Sel.Name == "Status" {
						suffix = "/status"
					} else {
						return true
					}
				}
				ty := typeOf(call.Args[1], vars)
				k, known := kinds[ty]
				if !known {
					return true
				}
				key := k[0] + "|" + k[1] + suffix + "|" + verb
				used[key] = append(used[key], filepath.Base(f)+":"+fset.Position(call.Pos()).String()[strings.LastIndex(fset.Position(call.Pos()).String(), ":")-3:])
				return true
			})
		}
	}
	if len(used) < 15 {
		t.Fatalf("the scan found only %d distinct writes: the test lost its footing", len(used))
	}
	var missing []string
	for key, where := range used {
		if !granted[key] {
			missing = append(missing, key+" (used in "+where[0]+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("the code writes %s, and no chart role grants it", m)
	}
}
