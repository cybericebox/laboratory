// Package runtime configures Go runtime parameters for containerised deployments.
// Import with a blank identifier to activate:
//
//	import _ "github.com/cybericebox/laboratory/pkg/runtime"
package runtime

import _ "go.uber.org/automaxprocs" // sets GOMAXPROCS from cgroup CPU quota
