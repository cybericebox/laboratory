package nstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// CI runs the privileged network tests only as `-test.run TestNetns` (hack/test-netns.sh). A test that uses this package but is named
// otherwise would be skipped there and never run anywhere, so every test that calls into nstest must be called TestNetns...
func TestEveryTestThatUsesNstestIsRunByTheNetnsJob(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".worktrees" || d.Name() == "bin" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil // a file that does not parse is not ours to judge here
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") || strings.HasPrefix(fn.Name.Name, "TestNetns") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "nstest" && sel.Sel.Name != "HelperProcess" {
					t.Errorf("%s: %s uses nstest but is not named TestNetns...: the netns job (-test.run TestNetns) never runs it", path, fn.Name.Name)
					return false
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
