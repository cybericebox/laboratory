package chart_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// grepTree returns every line under root (a file or a directory) that matches pattern, as "path:line: text", one per line; empty when none does.
// The checks of section 5 of the pools release are written with it: each row of the checklist is one call.
func grepTree(t *testing.T, root, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// readChart reads a file of the chart by its path inside charts/laboratory.
func readChart(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../charts/laboratory", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
