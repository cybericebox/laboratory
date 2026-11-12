//go:build !unix

package probe

func runResolveUser(string) int { return ExitIO }
func runPut(string) int         { return ExitIO }
func runGet(string) int         { return ExitIO }
