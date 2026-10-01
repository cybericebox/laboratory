package grpc

// copyLabels returns a copy of a label map (nil for none).
func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// mergeLabels adds the given labels to dst (allocating it when needed) and
// reports whether anything changed. A label is never removed this way.
func mergeLabels(dst map[string]string, add map[string]string) (map[string]string, bool) {
	changed := false
	for k, v := range add {
		if cur, ok := dst[k]; ok && cur == v {
			continue
		}
		if dst == nil {
			dst = map[string]string{}
		}
		dst[k] = v
		changed = true
	}
	return dst, changed
}
