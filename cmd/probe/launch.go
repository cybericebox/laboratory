package main

import (
	"fmt"
	"os"

	"github.com/cybericebox/laboratory/internal/launcher"
	"github.com/cybericebox/laboratory/internal/portkeeper"
)

// launchMain is "probe launch ...": the first process of a device container with a bootstrap list or a service command.
func launchMain(args []string) {
	os.Exit(launcher.Main(args, os.Stderr))
}

// resolveConsoleUser resolves the user of a console command from the container's own /etc/passwd and
// /etc/group, like the service user.
func resolveConsoleUser(user string) (portkeeper.Identity, error) {
	id, err := launcher.Accounts{Root: "/"}.Resolve(user)
	if err != nil {
		return portkeeper.Identity{}, err
	}
	return portkeeper.Identity{UID: id.UID, GID: id.GID, Groups: id.Groups, Home: id.Home}, nil
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
