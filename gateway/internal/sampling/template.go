package sampling

import "strings"

// The placeholder is the same shape Go's own router uses for a wildcard
// segment, so a templated endpoint on the dashboard matches the route the
// services register.
const placeholder = "{id}"

// Template turns a request path into the route behind it, so risk scoring
// groups by endpoint rather than by URL.
//
// Without it /api/products/1 and /api/products/2 are two endpoints with
// separate histories: a mismatch found on one says nothing about the other,
// and every individual product id looks rare because it is a small share of
// traffic on its own.
//
// It is deliberately conservative. A segment has to look like an identifier
// rather than merely be unfamiliar, so /api/products keeps its own score
// instead of being folded in with /api/products/{id}, and a path that is all
// fixed segments comes back untouched.
func Template(path string) string {
	if !strings.Contains(path, "/") {
		return path
	}
	segments := strings.Split(path, "/")
	changed := false
	for i, segment := range segments {
		if isIdentifier(segment) {
			segments[i] = placeholder
			changed = true
		}
	}
	if !changed {
		return path
	}
	return strings.Join(segments, "/")
}

// isIdentifier recognises the three shapes that turn up as path parameters:
// a number, a UUID, and the long opaque strings used for generated ids.
func isIdentifier(segment string) bool {
	if segment == "" || segment == placeholder {
		return false
	}
	var digits, letters, dashes int
	for _, r := range segment {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		case r == '-':
			dashes++
		default:
			return false
		}
	}
	if letters == 0 && dashes == 0 {
		return digits > 0
	}
	if isUUID(segment) {
		return true
	}
	// A generated id: long, no separators, and not a word. Short mixed
	// segments like "v2" or "api" stay as they are.
	return dashes == 0 && digits > 0 && len(segment) >= 16
}

func isUUID(segment string) bool {
	groups := strings.Split(segment, "-")
	want := []int{8, 4, 4, 4, 12}
	if len(groups) != len(want) {
		return false
	}
	for i, group := range groups {
		if len(group) != want[i] {
			return false
		}
		for _, r := range group {
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
