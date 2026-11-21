package main

import (
	"fmt"
	"os"

	"github.com/cybericebox/laboratory/internal/launcher"
)

// launchMain is "probe launch ...": the first process of a device container with a bootstrap list or a service command.
func launchMain(args []string) {
	os.Exit(launcher.Main(args, os.Stderr))
}

// stageMain is "probe stage SRC DIR": the init container that moves the bootstrap list from the Secret into the memory
// volume.
func stageMain(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: probe stage SRC DIR")
		os.Exit(launcher.ExitUsage)
	}
	if err := launcher.Stage(args[0], args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "probe stage:", err)
		os.Exit(1)
	}
}
