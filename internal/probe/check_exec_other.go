//go:build !unix

package probe

func runCheckExec(ExecRequest) ExecResponse {
	return ExecResponse{Error: "custom checks run on Linux only"}
}
