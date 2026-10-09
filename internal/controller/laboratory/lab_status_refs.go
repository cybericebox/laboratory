package laboratory

import "reflect"

// Informer list order is not stable. Compare complete reference values with
// multiplicity, without changing the published order on a real status update.
func sameRefsByName[T any](a, b []T, name func(T) string) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	if len(a) != len(b) || (a == nil) != (b == nil) {
		return false
	}
	remaining := make(map[string][]T, len(b))
	for _, ref := range b {
		key := name(ref)
		remaining[key] = append(remaining[key], ref)
	}
	for _, ref := range a {
		key := name(ref)
		candidates := remaining[key]
		found := false
		for i, candidate := range candidates {
			if reflect.DeepEqual(ref, candidate) {
				candidates[i] = candidates[len(candidates)-1]
				remaining[key] = candidates[:len(candidates)-1]
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
