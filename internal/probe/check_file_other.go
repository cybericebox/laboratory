//go:build !unix

package probe

func runCheckFile(FileRequest) FileResponse {
	return FileResponse{Error: "file checks run on Linux only"}
}
