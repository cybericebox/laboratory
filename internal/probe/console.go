package probe

import (
	"fmt"
	"os"
)

// The in-device side of the console (internal/deviceaccess). The device access service runs these through the container runtime's
// exec, in the device, with the rights of the console's user; the service itself never touches the file system of a device.
//
//	probe resolve-user NAME   answer the numeric ids of a user of the device as one JSON document
//	probe put PATH            write standard input to a regular file (the upload of the console)
//	probe get PATH            write the length of a regular file as 8 bytes (big endian), then its content (the download)
//
// A failure is told by the exit code only, never by text the service would have to trust.
const (
	CommandResolveUser = "resolve-user"
	CommandPut         = "put"
	CommandGet         = "get"
)

// Exit codes of put and get.
const (
	ExitUsage      = 2
	ExitNotFound   = 10
	ExitNotRegular = 11
	ExitDenied     = 12
	ExitIO         = 14
	// ExitNoUser is the exit code of resolve-user for a user the device does not know.
	ExitNoUser = 15
)

// RunConsoleCommand runs one of the three commands and returns the process exit code.
func RunConsoleCommand(cmd string, args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: probe", cmd, "<argument>")
		return ExitUsage
	}
	switch cmd {
	case CommandResolveUser:
		return runResolveUser(args[0])
	case CommandPut:
		return runPut(args[0])
	case CommandGet:
		return runGet(args[0])
	}
	return ExitUsage
}
