//go:build unix

package probe

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStdio runs fn with the given standard input and returns what it wrote to the standard output.
func withStdio(t *testing.T, in []byte, fn func() int) (int, []byte) {
	t.Helper()
	oldIn, oldOut := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	os.Stdin, os.Stdout = inR, outW
	go func() { _, _ = inW.Write(in); _ = inW.Close() }()
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(outR); done <- b }()
	code := fn()
	_ = outW.Close()
	return code, <-done
}

func TestPutWritesARegularFileAndRefusesTheRest(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if code, _ := withStdio(t, []byte("hello"), func() int { return runPut(target) }); code != 0 {
		t.Fatalf("put: %d", code)
	}
	if b, _ := os.ReadFile(target); string(b) != "hello" {
		t.Fatalf("content %q", b)
	}
	// an existing file is replaced, not appended to
	if code, _ := withStdio(t, []byte("hi"), func() int { return runPut(target) }); code != 0 {
		t.Fatalf("put again: %d", code)
	}
	if b, _ := os.ReadFile(target); string(b) != "hi" {
		t.Fatalf("content %q", b)
	}
	// a link in the last component is not followed
	secret := filepath.Join(dir, "secret")
	_ = os.WriteFile(secret, []byte("keep"), 0o600)
	link := filepath.Join(dir, "link")
	_ = os.Symlink(secret, link)
	if code, _ := withStdio(t, []byte("overwritten"), func() int { return runPut(link) }); code == 0 {
		t.Fatal("put followed a symbolic link")
	}
	if b, _ := os.ReadFile(secret); string(b) != "keep" {
		t.Fatalf("the target of the link changed: %q", b)
	}
	if code, _ := withStdio(t, []byte("x"), func() int { return runPut(dir) }); code == 0 {
		t.Fatal("put wrote to a directory")
	}
	if code, _ := withStdio(t, []byte("x"), func() int { return runPut(filepath.Join(dir, "missing", "f")) }); code != ExitNotFound {
		t.Fatalf("missing parent: %d", code)
	}
}

func TestGetSendsTheLengthThenTheContentAndRefusesTheRest(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	content := bytes.Repeat([]byte("abc"), 1000)
	_ = os.WriteFile(f, content, 0o600)
	code, out := withStdio(t, nil, func() int { return runGet(f) })
	if code != 0 || len(out) != 8+len(content) || binary.BigEndian.Uint64(out) != uint64(len(content)) || !bytes.Equal(out[8:], content) {
		t.Fatalf("get: %d, %d bytes", code, len(out))
	}
	if code, out := withStdio(t, nil, func() int { return runGet(dir) }); code != ExitNotRegular || len(out) != 0 {
		t.Fatalf("directory: %d, %d bytes", code, len(out))
	}
	if code, out := withStdio(t, nil, func() int { return runGet(filepath.Join(dir, "none")) }); code != ExitNotFound || len(out) != 0 {
		t.Fatalf("missing: %d, %d bytes", code, len(out))
	}
	if code, out := withStdio(t, nil, func() int { return runGet("/dev/null") }); code != ExitNotRegular || len(out) != 0 {
		t.Fatalf("device node: %d, %d bytes", code, len(out))
	}
	fifo := filepath.Join(dir, "fifo")
	if err := mkfifo(fifo); err == nil {
		if code, _ := withStdio(t, nil, func() int { return runGet(fifo) }); code == 0 {
			t.Fatal("a FIFO was read (and would have blocked a real exec)")
		}
	}
}

func TestResolveUser(t *testing.T) {
	if _, err := os.Stat("/etc/passwd"); err != nil || !strings.Contains(readFile("/etc/passwd"), "root:") {
		t.Skip("no /etc/passwd with root")
	}
	code, out := withStdio(t, nil, func() int { return runResolveUser("root") })
	if code != 0 || !strings.Contains(string(out), `"uid":0`) {
		t.Fatalf("root: %d %s", code, out)
	}
	if code, _ := withStdio(t, nil, func() int { return runResolveUser("no-such-user-cib") }); code != ExitNoUser {
		t.Fatalf("unknown user: %d", code)
	}
	// a number without an entry is a user too
	code, out = withStdio(t, nil, func() int { return runResolveUser("54321:54322") })
	if code != 0 || !strings.Contains(string(out), `"uid":54321`) || !strings.Contains(string(out), `"gid":54322`) {
		t.Fatalf("numbers: %d %s", code, out)
	}
	if code, _ := withStdio(t, nil, func() int { return runResolveUser("root:no-such-group-cib") }); code != ExitNoUser {
		t.Fatalf("unknown group: %d", code)
	}
}

func TestRunConsoleCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"a", "b"}} {
		if RunConsoleCommand(CommandPut, args) != ExitUsage {
			t.Errorf("%v accepted", args)
		}
	}
	if RunConsoleCommand("other", []string{"x"}) != ExitUsage {
		t.Error("unknown command accepted")
	}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }
