//go:build unix

package probe

import (
	"io"
	"os"
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

func TestConsoleResolveUser(t *testing.T) {
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
		if RunConsoleCommand(CommandResolveUser, args) != ExitUsage {
			t.Errorf("%v accepted", args)
		}
	}
	if RunConsoleCommand("other", []string{"x"}) != ExitUsage {
		t.Error("unknown command accepted")
	}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }
