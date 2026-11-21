//go:build !unix

package probe

// Observe: the built-in observations run on Linux devices only.
func Observe(ObserveRequest) ObserveResponse {
	return ObserveResponse{Error: "built-in observations run on Linux only"}
}

// RunPermChild is the child of a permission test; it exists on Linux only.
func RunPermChild(string) int { return 2 }
