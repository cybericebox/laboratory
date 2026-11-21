package chart_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The contract between the chart and the code: the environment a component's binary reads is the environment its container is given. Each
// cmd package's configuration is read from the source (the `env` struct tags of the caarlos0/env parser and the literal os.Getenv names of
// every package the binary links), and compared with what the chart renders for the container that runs it:
//   - an env var the chart sets that no package of the binary reads is dead configuration;
//   - an env var the binary requires (notEmpty or required, with no default) that the chart does not set is a pod that will not start;
//   - a file the binary is told to read (an absolute path in an env value, or the default of one the chart leaves alone) must be inside a
//     volume mounted in the container.

type envSpec struct {
	required bool
	def      string
	hasDef   bool
}

const modulePrefix = "github.com/cybericebox/laboratory/"

type goPkg struct {
	importPath, dir string
	files, imports  []string
}

func listPkgs(t *testing.T, deps bool, pkgs ...string) []goPkg {
	t.Helper()
	args := []string{"list"}
	if deps {
		args = append(args, "-deps")
	}
	args = append(args, "-f", `{{if not .Standard}}{{.ImportPath}}|{{.Dir}}|{{join .GoFiles ","}}|{{join .Imports ","}}{{end}}`)
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Dir = "../.."
	cmd.Env = append(cmd.Environ(), "GOOS=linux", "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %v: %v", args, err)
	}
	var res []goPkg
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 4 || !strings.HasPrefix(parts[0], modulePrefix) {
			continue
		}
		res = append(res, goPkg{parts[0], parts[1], strings.Split(parts[2], ","), strings.Split(parts[3], ",")})
	}
	return res
}

// stringConsts are the string constants of the packages (by name): os.Getenv(EnvEnabled) names its variable through one.
var stringConsts = map[string]string{}

func collectConsts(t *testing.T, p goPkg) {
	t.Helper()
	fset := token.NewFileSet()
	for _, f := range p.files {
		file, err := parser.ParseFile(fset, filepath.Join(p.dir, f), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								stringConsts[n.Name] = v
							}
						}
					}
				}
			}
		}
	}
}

var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,}$`)

// scanEnv adds to res the environment variables a package's files read: the `env` struct tags (tags), and the literal names given to
// os.Getenv and os.LookupEnv (getenv).
func scanEnv(t *testing.T, p goPkg, res map[string]envSpec, tags, getenv bool) {
	t.Helper()
	fset := token.NewFileSet()
	for _, f := range p.files {
		file, err := parser.ParseFile(fset, filepath.Join(p.dir, f), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Field:
				if !tags || x.Tag == nil {
					return true
				}
				raw, _ := strconv.Unquote(x.Tag.Value)
				tag := reflect.StructTag(raw)
				name, ok := tag.Lookup("env")
				if !ok {
					return true
				}
				name, opts, _ := strings.Cut(name, ",")
				def, hasDef := tag.Lookup("envDefault")
				res[name] = envSpec{required: (strings.Contains(opts, "notEmpty") || strings.Contains(opts, "required")) && !hasDef, def: def, hasDef: hasDef}
			case *ast.CallExpr:
				if !getenv || len(x.Args) == 0 {
					return true
				}
				// os.Getenv, os.LookupEnv, and the small helpers a package wraps them in (envOrDefault, getenv, ...): the variable is the first
				// argument, a literal or a string constant
				var callee string
				switch f := x.Fun.(type) {
				case *ast.SelectorExpr:
					callee = f.Sel.Name
				case *ast.Ident:
					callee = f.Name
				}
				if !strings.Contains(strings.ToLower(callee), "env") {
					return true
				}
				var name string
				switch a := x.Args[0].(type) {
				case *ast.BasicLit:
					name, _ = strconv.Unquote(a.Value)
				case *ast.Ident:
					name = stringConsts[a.Name]
				case *ast.SelectorExpr:
					name = stringConsts[a.Sel.Name]
				}
				if envName.MatchString(name) {
					if _, have := res[name]; !have {
						res[name] = envSpec{}
					}
				}
			}
			return true
		})
	}
}

// scanStruct adds the tags of the struct type root of the package and of the structs of the same package it holds (by field type), and
// nothing else of the package: a package that holds the configuration of two binaries is read once for each.
func scanStruct(t *testing.T, p goPkg, root string, res map[string]envSpec) {
	t.Helper()
	fset := token.NewFileSet()
	structs := map[string]*ast.StructType{}
	for _, f := range p.files {
		file, err := parser.ParseFile(fset, filepath.Join(p.dir, f), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok {
					if st, ok := ts.Type.(*ast.StructType); ok {
						structs[ts.Name.Name] = st
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		st, ok := structs[name]
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		for _, f := range st.Fields.List {
			if f.Tag != nil {
				raw, _ := strconv.Unquote(f.Tag.Value)
				tag := reflect.StructTag(raw)
				if v, ok := tag.Lookup("env"); ok {
					n, opts, _ := strings.Cut(v, ",")
					def, hasDef := tag.Lookup("envDefault")
					res[n] = envSpec{required: (strings.Contains(opts, "notEmpty") || strings.Contains(opts, "required")) && !hasDef, def: def, hasDef: hasDef}
				}
			}
			switch x := f.Type.(type) {
			case *ast.Ident:
				visit(x.Name)
			case *ast.ArrayType:
				if id, ok := x.Elt.(*ast.Ident); ok {
					visit(id.Name)
				}
			}
		}
	}
	visit(root)
}

// declaredEnv is the environment the binary of the cmd package reads: the tags of the configuration struct it loads (the packages in
// cfgPkgs, and the module packages those import, where its embedded structs live) and every literal os.Getenv of the packages it links.
func declaredEnv(t *testing.T, pkg string, cfgPkgs ...string) map[string]envSpec {
	t.Helper()
	res := map[string]envSpec{}
	deps := listPkgs(t, true, pkg)
	for _, p := range deps {
		collectConsts(t, p)
	}
	for _, p := range deps {
		scanEnv(t, p, res, false, true)
	}
	seen := map[string]bool{}
	var structs []string // "pkg:Type" entries
	var plain []string
	for _, c := range cfgPkgs {
		if i := strings.Index(c, ":"); i >= 0 {
			structs = append(structs, c)
			plain = append(plain, c[:i])
		} else {
			plain = append(plain, c)
		}
	}
	for _, e := range structs {
		i := strings.Index(e, ":")
		for _, p := range listPkgs(t, false, e[:i]) {
			scanStruct(t, p, e[i+1:], res)
		}
	}
	for _, p := range listPkgs(t, false, append([]string{pkg}, plain...)...) {
		if seen[p.importPath] {
			continue
		}
		seen[p.importPath] = true
		if len(structs) == 0 || !strings.HasPrefix(strings.TrimPrefix(p.importPath, modulePrefix), "internal/proxy") || p.importPath != modulePrefix+"internal/proxy" {
			scanEnv(t, p, res, true, false)
		}
		if p.importPath == modulePrefix+strings.TrimPrefix(pkg, "./") {
			continue // the cmd package itself: its imports are the whole program
		}
		var imps []string
		for _, imp := range p.imports {
			if strings.HasPrefix(imp, modulePrefix) {
				imps = append(imps, imp)
			}
		}
		if len(imps) > 0 {
			for _, q := range listPkgs(t, false, imps...) {
				if !seen[q.importPath] {
					seen[q.importPath] = true
					scanEnv(t, q, res, true, false)
				}
			}
		}
	}
	return res
}

// containerEnv is the environment a container is given: its env entries and the keys of the ConfigMaps it takes with envFrom.
func containerEnv(ns string, c corev1.Container, configMaps map[string]map[string]any) (res map[string]string, from map[string]string) {
	res, from = map[string]string{}, map[string]string{}
	for _, ef := range c.EnvFrom {
		if ef.ConfigMapRef == nil {
			continue
		}
		cm, ok := configMaps[ns+"/"+ef.ConfigMapRef.Name]
		if !ok {
			continue
		}
		data, _ := cm["data"].(map[string]any)
		for k, v := range data {
			s, _ := v.(string)
			res[ef.Prefix+k] = s
			from[ef.Prefix+k] = ns + "/" + ef.ConfigMapRef.Name
		}
	}
	for _, e := range c.Env {
		res[e.Name] = e.Value
		delete(from, e.Name)
	}
	return res, from
}

// readsPath says whether a value is a path the binary opens (not a URL, not a flag).
var absPath = regexp.MustCompile(`^/(etc|run|var|usr|opt|sys|proc)/[^\s,]+$`)

func mounted(c corev1.Container, path string) bool {
	for _, vm := range c.VolumeMounts {
		if path == vm.MountPath || strings.HasPrefix(path, strings.TrimSuffix(vm.MountPath, "/")+"/") {
			return true
		}
	}
	return false
}

type contractCase struct {
	workload  string // Kind/namespace/name
	container string
	pkg       string   // the cmd package that is the container's binary
	cfg       []string // the packages that hold the configuration struct it loads
}

var contractCases = []contractCase{
	{"Deployment/laboratory-system/laboratory-controller-manager", "manager", "./internal/cmds/manager", []string{"./internal/operator"}},
	{"Deployment/laboratory-agent/laboratory-agent", "agent", "./internal/cmds/agent", []string{"./internal/agent/config"}},
	{"Deployment/laboratory-proxy/laboratory-proxy", "l7", "./internal/cmds/proxyl7", []string{"./internal/proxy:L7Config"}},
	{"DaemonSet/laboratory-wg/laboratory-proxy-wg", "wg-demux", "./internal/cmds/proxywg", []string{"./internal/proxy:WGConfig"}},
	{"DaemonSet/laboratory-system/laboratory-node-agent", "node-agent", "./internal/cmds/nodeagentd", []string{"./internal/nodeagent"}},
	{"DaemonSet/laboratory-system/laboratory-node-agent", "install-cni-conf", "./internal/cmds/installcni", nil},
	{"DaemonSet/laboratory-system/laboratory-node-state", "node-state", "./internal/cmds/nodestated", []string{"./internal/nodeagent"}},
	{"DaemonSet/laboratory-device-access/laboratory-device-access", "device-access", "./internal/cmds/deviceaccessd", nil},
	{"DaemonSet/laboratory-system/laboratory-checker", "checker", "./internal/cmds/checkerd", nil},
	{"Deployment/laboratory-system/laboratory-registry", "gate", "./internal/cmds/registrygated", nil},
}

func TestChartEnvironmentMatchesWhatTheCodeReads(t *testing.T) {
	for _, mode := range []string{"replicas", "daemonset"} {
		out, err := helmTemplate(t, append([]string{"--set", "proxy.mode=" + mode}, everything...)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		all := docs(t, out)
		configMaps := map[string]map[string]any{}
		for _, d := range all {
			if d["kind"] == "ConfigMap" {
				md, _ := d["metadata"].(map[string]any)
				ns, _ := md["namespace"].(string)
				configMaps[ns+"/"+md["name"].(string)] = d
			}
		}
		pods := podsOf(t, out)
		// a ConfigMap taken by several containers (laboratory-config: what the operator and the agent both read) is one map: a key is dead only
		// when none of them reads it
		readers := map[string][]map[string]envSpec{}
		find := func(c contractCase, spec corev1.PodSpec) *corev1.Container {
			for i := range spec.Containers {
				if spec.Containers[i].Name == c.container {
					return &spec.Containers[i]
				}
			}
			for i := range spec.InitContainers {
				if spec.InitContainers[i].Name == c.container {
					return &spec.InitContainers[i]
				}
			}
			return nil
		}
		for _, c := range contractCases {
			if spec, ok := pods[c.workload]; ok {
				if cont := find(c, spec); cont != nil {
					d := declaredEnv(t, c.pkg, c.cfg...)
					for _, ef := range cont.EnvFrom {
						if ef.ConfigMapRef != nil {
							key := strings.Split(c.workload, "/")[1] + "/" + ef.ConfigMapRef.Name
							readers[key] = append(readers[key], d)
						}
					}
				}
			}
		}
		for _, c := range contractCases {
			spec, ok := pods[c.workload]
			if !ok {
				if mode == "daemonset" && strings.HasSuffix(c.workload, "laboratory-proxy") {
					continue // the proxy runs as a DaemonSet in this mode (checked below)
				}
				t.Errorf("%s: not rendered (workloads: %v)", c.workload, keysOf(pods))
				continue
			}
			cont := find(c, spec)
			if cont == nil {
				t.Errorf("%s: no container %q", c.workload, c.container)
				continue
			}
			declared := declaredEnv(t, c.pkg, c.cfg...)
			given, from := containerEnv(strings.Split(c.workload, "/")[1], *cont, configMaps)
			for name := range given {
				if _, ok := declared[name]; !ok {
					// an entry another entry's value refers to ($(POD_IP)) is read by the kubelet, not by the binary
					referred := false
					for _, v := range given {
						if strings.Contains(v, "$("+name+")") {
							referred = true
						}
					}
					if referred {
						continue
					}
					if cm := from[name]; cm != "" {
						read := false
						for _, d := range readers[cm] {
							if _, ok := d[name]; ok {
								read = true
							}
						}
						if read {
							continue
						}
					}
					t.Errorf("%s/%s (%s): the chart sets %s, nothing the binary links reads it", c.workload, c.container, mode, name)
				}
			}
			for name, spec := range declared {
				if _, set := given[name]; spec.required && !set {
					t.Errorf("%s/%s (%s): the binary requires %s, the chart does not set it", c.workload, c.container, mode, name)
				}
			}
			for name, spec := range declared {
				val, set := given[name]
				if !set {
					if len(c.cfg) > 0 {
						continue // a shared configuration struct holds defaults the binary may never use; only what the chart sets is checked
					}
					val = spec.def
				}
				if absPath.MatchString(val) && !mounted(*cont, val) && !isRuntimePath(val) && !writtenNotRead[c.container+"/"+name] {
					t.Errorf("%s/%s (%s): %s=%s is read from a path no volume provides", c.workload, c.container, mode, name, val)
				}
			}
		}
	}
}

// writtenNotRead are the paths a container is given to write into a file for the host, not to open (the CNI config the init container installs
// names the node-agent's socket, which is the host's and not mounted there).
var writtenNotRead = map[string]bool{"install-cni-conf/GRPC_SOCK": true}

// isRuntimePath is a path that exists in the image or on the host without a volume (the service account's token, /proc, /sys, /run of the pod).
func isRuntimePath(p string) bool {
	for _, pre := range []string{"/proc/", "/sys/", "/etc/resolv.conf", "/var/run/secrets/kubernetes.io/", "/usr/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
