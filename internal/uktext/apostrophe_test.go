package uktext

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Ukrainian words must use the modifier letter apostrophe (U+02BC), never ' or ’.
var wrongApostrophe = regexp.MustCompile("[а-яіїєґА-ЯІЇЄҐ]['’][а-яіїєґА-ЯІЇЄҐ]")

func TestNoWrongApostropheInServedTexts(t *testing.T) {
	root := filepath.Join("..", "..")
	skipDirs := map[string]bool{".git": true, ".worktrees": true, "node_modules": true, "bin": true, "docs": true}
	exts := map[string]bool{".go": true, ".html": true, ".json": true, ".js": true, ".css": true}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[filepath.Ext(path)] || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if m := wrongApostrophe.FindString(line); m != "" {
				t.Errorf("%s:%d: %q: use ʼ (U+02BC) inside Ukrainian words", path, i+1, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
