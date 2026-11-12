// probe is the readiness probe of a device pod (see package internal/probe). Two commands:
//
//	probe install DIR                            copy this program to DIR/probe (the init container)
//	probe ready [-file F] [-port N -scheme S]    exit 0 when the container is ready (the kubelet's exec probe)
//	probe check-exec JSON | check-file JSON      one observation of a lab check, answered as JSON (internal/probe)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/cybericebox/laboratory/internal/probe"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe install DIR | probe ready [-file F] [-port N -scheme tcp|http|https]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "install":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: probe install DIR")
			os.Exit(2)
		}
		self, err := os.Executable()
		if err == nil {
			err = probe.Install(self, os.Args[2])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe install:", err)
			os.Exit(1)
		}
	case "launch":
		launchMain(os.Args[2:])
	case "stage":
		stageMain(os.Args[2:])
	case "ready":
		fs := flag.NewFlagSet("ready", flag.ExitOnError)
		file := fs.String("file", "", "marker file that must exist")
		port := fs.Int("port", 0, "port that must answer")
		scheme := fs.String("scheme", probe.SchemeTCP, "tcp, http or https")
		_ = fs.Parse(os.Args[2:])
		c := probe.Check{File: *file, Port: *port, Scheme: *scheme}
		if err := c.Run(context.Background(), probe.LocalAddrs); err != nil {
			fmt.Fprintln(os.Stderr, "not ready:", err)
			os.Exit(1)
		}
	case probe.CommandCheckExec, probe.CommandCheckFile, probe.CommandCheckObserve, probe.CommandCheckPerm:
		os.Exit(probe.RunCheckCommand(os.Args[1], os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}
