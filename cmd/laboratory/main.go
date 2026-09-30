// laboratory is the single binary behind every Laboratory image. It only routes:
// the first argument (or the name the binary is invoked as, for symlinks and for the
// cni-gate copy on the host) selects the component, whose logic lives in internal/cmds.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// commands is filled by init() in the commands_*.go files.
var commands = map[string]func(){}

func main() {
	name := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if _, ok := commands[name]; !ok {
		if len(args) == 0 {
			usage()
		}
		name, args = args[0], args[1:]
	}
	run, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "laboratory: unknown command %q\n", name)
		usage()
	}
	// The component parses its own flags from os.Args as if it were the only binary.
	os.Args = append([]string{name}, args...)
	run()
}

func usage() {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(os.Stderr, "usage: laboratory <command> [args]\ncommands: %s\n", strings.Join(names, ", "))
	os.Exit(2)
}
