// Package multicall routes one binary to several components of the same domain:
// the first argument selects the component, or the name the binary is invoked as
// (for example the cni-gate copy that the node-agent installs on the host).
package multicall

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Run starts the selected component. Each component parses its own flags from
// os.Args as if it were the only binary, so os.Args is rewritten to start at it.
func Run(commands map[string]func()) {
	name := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if _, ok := commands[name]; !ok {
		if len(args) == 0 {
			usage(name, commands)
		}
		name, args = args[0], args[1:]
	}
	run, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n", name)
		usage(filepath.Base(os.Args[0]), commands)
	}
	os.Args = append([]string{name}, args...)
	run()
}

func usage(self string, commands map[string]func()) {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(os.Stderr, "usage: %s <command> [args]\ncommands: %s\n", self, strings.Join(names, ", "))
	os.Exit(2)
}
