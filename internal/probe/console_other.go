//go:build !unix

package probe

func runResolveUser(string) int { return ExitIO }
