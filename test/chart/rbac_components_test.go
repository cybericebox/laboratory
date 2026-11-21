package chart_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The rights of every component besides the operator (whose markers and writes are checked in rbac_usage_test.go): what its code asks of the API
// server is granted by the Roles and ClusterRoles bound to its ServiceAccount. The scan reads the calls of the typed client (client-go's
// CoreV1().Secrets(ns).Delete) and of the controller-runtime client (Get, List, Create, Update, Patch, Delete with an object whose type is known).
// A call whose object type cannot be told from the function is not seen: this is a net for the drift the reviews found (a verb the code uses and the
// chart never granted, a resource it reads that a role does not name), not a proof.

// boundRules is, by ServiceAccount ("namespace/name"), every rule of every Role and ClusterRole the chart binds to it, as "group|resource|verb".
func boundRules(t *testing.T) map[string]map[string]bool {
	t.Helper()
	var out string
	for _, mode := range []string{"replicas"} {
		o, err := helmTemplate(t, append([]string{"--set", "proxy.mode=" + mode}, everything...)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, o)
		}
		out = o
	}
	all := docs(t, out)
	rules := map[string]map[string]bool{} // "Kind/namespace/name" of a role -> its rules
	for _, d := range all {
		if k := d["kind"]; k != "Role" && k != "ClusterRole" {
			continue
		}
		md, _ := d["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		set := map[string]bool{}
		for _, r := range rulesOf(t, d) {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, v := range r.Verbs {
						set[g+"|"+res+"|"+v] = true
					}
				}
			}
		}
		rules[d["kind"].(string)+"/"+ns+"/"+md["name"].(string)] = set
	}
	res := map[string]map[string]bool{}
	// the ClusterRoles the operator binds in each group namespace when it makes the namespace (RoleBindings the chart cannot render)
	for sa, roles := range runtimeBound {
		res[sa] = map[string]bool{}
		for _, r := range roles {
			for rule := range rules["ClusterRole//"+r] {
				res[sa][rule] = true
			}
		}
	}
	for _, d := range all {
		if k := d["kind"]; k != "RoleBinding" && k != "ClusterRoleBinding" {
			continue
		}
		md, _ := d["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		ref, _ := d["roleRef"].(map[string]any)
		refNS := ""
		if ref["kind"] == "Role" {
			refNS = ns
		}
		set := rules[ref["kind"].(string)+"/"+refNS+"/"+ref["name"].(string)]
		subjects, _ := d["subjects"].([]any)
		for _, s := range subjects {
			sm, _ := s.(map[string]any)
			if sm["kind"] != "ServiceAccount" {
				continue
			}
			sns, _ := sm["namespace"].(string)
			key := sns + "/" + sm["name"].(string)
			if res[key] == nil {
				res[key] = map[string]bool{}
			}
			for r := range set {
				res[key][r] = true
			}
		}
	}
	return res
}

// runtimeBound are the ClusterRoles the operator binds to a ServiceAccount in every group namespace.
var runtimeBound = map[string][]string{
	"laboratory-agent/laboratory-agent": {"laboratory-agent-role"},
	"laboratory-proxy/laboratory-proxy": {"laboratory-proxy-reports"},
}

var clientGroups = map[string]string{
	"CoreV1": "", "AppsV1": "apps", "RbacV1": "rbac.authorization.k8s.io", "NetworkingV1": "networking.k8s.io", "CoordinationV1": "coordination.k8s.io",
	"DiscoveryV1": "discovery.k8s.io", "AuthenticationV1": "authentication.k8s.io", "EventsV1": "events.k8s.io", "PolicyV1": "policy",
	"LaboratoryV1alpha1": "laboratory.cybericebox.com",
}

var verbOf = map[string]string{"Get": "get", "List": "list", "Watch": "watch", "Create": "create", "Update": "update", "Patch": "patch", "Delete": "delete",
	"DeleteCollection": "deletecollection"}

// usedCalls is every "group|resource|verb" the code of dirs asks of the API server, with where.
func usedCalls(t *testing.T, dirs ...string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	used := map[string][]string{}
	add := func(key string, pos token.Pos) {
		p := fset.Position(pos)
		used[key] = append(used[key], filepath.Base(p.Filename)+":"+strconv.Itoa(p.Line))
	}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join("../..", dir, "*.go"))
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
					case *ast.ValueSpec:
						for _, name := range x.Names {
							if x.Type != nil {
								vars[name.Name] = selName(x.Type)
							}
						}
					case *ast.AssignStmt:
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
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					name := sel.Sel.Name
					// the typed client: <client>.CoreV1().Secrets(ns).Delete(...)
					if verb, isVerb := verbOf[name]; isVerb || name == "UpdateStatus" {
						if rc, ok := sel.X.(*ast.CallExpr); ok {
							if rsel, ok := rc.Fun.(*ast.SelectorExpr); ok {
								if gc, ok := rsel.X.(*ast.CallExpr); ok {
									if gsel, ok := gc.Fun.(*ast.SelectorExpr); ok {
										if group, known := clientGroups[gsel.Sel.Name]; known {
											res := strings.ToLower(rsel.Sel.Name)
											if lit, ok := call.Args[len(call.Args)-1].(*ast.BasicLit); ok && name == "Patch" && lit.Value == `"status"` {
												add(group+"|"+res+"/status|patch", call.Pos()) // Patch(ctx, name, type, data, opts, "status")
											} else if name == "UpdateStatus" {
												add(group+"|"+res+"/status|update", call.Pos())
											} else {
												add(group+"|"+res+"|"+verb, call.Pos())
											}
											return true
										}
									}
								}
							}
						}
					}
					// the controller-runtime client: r.Get(ctx, key, &T{}), r.List(ctx, &list), r.Create(ctx, obj), ...
					verb, isVerb := verbOf[name]
					if !isVerb {
						return true
					}
					suffix := ""
					if inner, ok := sel.X.(*ast.CallExpr); ok { // r.Status().Patch(...)
						if is, ok := inner.Fun.(*ast.SelectorExpr); ok && is.Sel.Name == "Status" {
							suffix = "/status"
						}
					}
					idx := 1
					if name == "Get" {
						idx = 2
					}
					if len(call.Args) <= idx {
						return true
					}
					ty := typeOf(call.Args[idx], vars)
					if name == "List" {
						ty = strings.TrimSuffix(ty, "List")
					}
					if k, known := kinds[ty]; known {
						add(k[0]+"|"+k[1]+suffix+"|"+verb, call.Pos())
					}
					return true
				})
			}
		}
	}
	return used
}

// covers: a read is covered by get, list or watch (a cached reader needs list and watch), anything else by its own verb.
func covers(rules map[string]bool, key string) bool {
	if rules[key] {
		return true
	}
	parts := strings.Split(key, "|")
	if len(parts) != 3 {
		return false
	}
	if parts[2] == "get" {
		return rules[parts[0]+"|"+parts[1]+"|list"] || rules[parts[0]+"|"+parts[1]+"|watch"]
	}
	return false
}

var rbacComponents = []struct {
	name string
	sa   string
	dirs []string
}{
	{"agent", "laboratory-agent/laboratory-agent", []string{"internal/agent/grpc", "internal/cmds/agent"}},
	{"node-agent", "laboratory-system/laboratory-node-agent", []string{"internal/nodeagent", "internal/cmds/nodeagentd", "internal/imagepull"}},
	{"node-state", "laboratory-system/laboratory-node-state", []string{"internal/devicestate", "internal/cmds/nodestated"}},
	{"checker", "laboratory-system/laboratory-checker", []string{"internal/cmds/checkerd", "internal/labchecks"}},
	{"device-access", "laboratory-device-access/laboratory-device-access", []string{"internal/cmds/deviceaccessd", "internal/deviceaccess"}},
	{"l7 proxy", "laboratory-proxy/laboratory-proxy", []string{"internal/proxy/l7", "internal/cmds/proxyl7"}},
	{"wg demux", "laboratory-wg/laboratory-proxy-wg", []string{"internal/proxy/demux", "internal/cmds/proxywg"}},
}

func TestChartRolesCoverWhatEveryComponentCallsTheAPIFor(t *testing.T) {
	bound := boundRules(t)
	for _, c := range rbacComponents {
		rules := bound[c.sa]
		used := usedCalls(t, c.dirs...)
		var missing []string
		for key, where := range used {
			if !covers(rules, key) {
				missing = append(missing, key+" ("+where[0]+")")
			}
		}
		sort.Strings(missing)
		for _, m := range missing {
			t.Errorf("%s: the code calls %s, no role bound to %s grants it", c.name, m, c.sa)
		}
	}
}

// The L7 proxy routes a web request to the address in the status of the device, so it reads devices whether or not the terminal is on; the gate
// reads exactly what its schedule is made of; the agent writes a lab's upstream credentials by delete and create, and reads nothing there.
func TestRightsThatTheReviewsFound(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/clusterrole.yaml", "--set", "terminal.enabled=false")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var devices bool
	for _, d := range docs(t, out) {
		for _, r := range rulesOf(t, d) {
			for _, res := range r.Resources {
				if res == "devices" && len(r.Verbs) == 3 {
					devices = true
				}
			}
		}
	}
	if !devices {
		t.Error("the L7 proxy has no right to read devices with the terminal off: web routes need them")
	}
	out, err = helmTemplate(t, "-s", "templates/registry/rbac.yaml", "-s", "templates/upstream-namespace.yaml", "--set", "agent.enabled=true")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, d := range docs(t, out) {
		md, _ := d["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		for _, r := range rulesOf(t, d) {
			for _, res := range r.Resources {
				if name == "laboratory-registry-gate" && (res == "labs" || res == "secrets") {
					t.Errorf("the gate's cluster role names %s: it reads secrets of one namespace only and no Lab", res)
				}
				if name == "laboratory-agent-upstream" && res == "secrets" && strings.Join(r.Verbs, ",") != "create,delete" {
					t.Errorf("the agent's right on the upstream credentials is %v, the code deletes and creates", r.Verbs)
				}
			}
		}
	}
}
