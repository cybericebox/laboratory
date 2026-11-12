package probe

import (
	"fmt"
	"os"
)

// The in-device side of the console (internal/deviceaccess). The device access service runs these through the container runtime's
// exec, in the device, with the rights of the console's user; the service itself never touches the file system of a device.
//
//	probe resolve-user NAME   answer the numeric ids of a user of the device as one JSON document
//
// A failure is told by the exit code only, never by text the service would have to trust.
const (
	CommandResolveUser = "resolve-user"
)

// Exit codes of the console commands.
const (
	ExitUsage = 2
	ExitIO    = 14
	// ExitNoUser is the exit code of resolve-user for a user the device does not know.
	ExitNoUser = 15
)

// RunConsoleCommand runs the console command and returns the process exit code.
func RunConsoleCommand(cmd string, args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: probe", cmd, "<argument>")
		return ExitUsage
	}
	switch cmd {
	case CommandResolveUser:
		return runResolveUser(args[0])
	}
	return ExitUsage
}
