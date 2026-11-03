package update

import (
	"fmt"
	"strings"
)

type semanticVersion struct {
	major      string
	minor      string
	patch      string
	prerelease []string
}

// parseSemanticVersion parses a Semantic Versioning 2.0 version with an optional v prefix.
func parseSemanticVersion(input string) (semanticVersion, error) {
	var version semanticVersion
	s := strings.TrimSpace(input)
	s = strings.TrimPrefix(s, versionPrefix)
	if s == "" {
		return version, fmt.Errorf("empty version")
	}
	if before, build, found := strings.Cut(s, "+"); found {
		if !validIdentifiers(build, false) {
			return version, fmt.Errorf("invalid build metadata")
		}
		s = before
	}
	if before, pre, found := strings.Cut(s, "-"); found {
		if !validIdentifiers(pre, true) {
			return version, fmt.Errorf("invalid prerelease")
		}
		version.prerelease = strings.Split(pre, ".")
		s = before
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version, fmt.Errorf("version must have major, minor, and patch numbers")
	}
	for _, part := range parts {
		if !validNumericIdentifier(part) {
			return version, fmt.Errorf("invalid numeric identifier %q", part)
		}
	}
	version.major, version.minor, version.patch = parts[0], parts[1], parts[2]
	return version, nil
}

// validIdentifiers validates dot-separated prerelease or build metadata identifiers.
func validIdentifiers(s string, prerelease bool) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '-' {
				return false
			}
		}
		if prerelease && isNumeric(part) && !validNumericIdentifier(part) {
			return false
		}
	}
	return true
}

// validNumericIdentifier rejects empty, non-numeric, and zero-padded numeric identifiers.
func validNumericIdentifier(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	return isNumeric(s)
}

// isNumeric reports whether every rune in a non-empty string is an ASCII digit.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// compareSemanticVersions returns -1, 0, or 1 according to Semantic Versioning precedence.
func compareSemanticVersions(a, b semanticVersion) int {
	for _, pair := range [][2]string{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if cmp := compareNumeric(pair[0], pair[1]); cmp != 0 {
			return cmp
		}
	}
	if len(a.prerelease) == 0 && len(b.prerelease) != 0 {
		return 1
	}
	if len(a.prerelease) != 0 && len(b.prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(a.prerelease) && i < len(b.prerelease); i++ {
		x, y := a.prerelease[i], b.prerelease[i]
		xNumeric, yNumeric := isNumeric(x), isNumeric(y)
		switch {
		case xNumeric && yNumeric:
			if cmp := compareNumeric(x, y); cmp != 0 {
				return cmp
			}
		case xNumeric:
			return -1
		case yNumeric:
			return 1
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	switch {
	case len(a.prerelease) < len(b.prerelease):
		return -1
	case len(a.prerelease) > len(b.prerelease):
		return 1
	default:
		return 0
	}
}

// compareNumeric compares unsigned decimal integers without converting them to machine integers.
func compareNumeric(a, b string) int {
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
