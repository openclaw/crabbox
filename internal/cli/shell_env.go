package cli

import "strings"

// ValidShellEnvName accepts portable ASCII shell environment identifiers without
// trimming. Callers own whether an invalid name is rejected or omitted.
func ValidShellEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// IsShellEnvAssignment classifies the name before the first '='. Assignment
// values are shell data and do not participate in identifier validation.
func IsShellEnvAssignment(word string) bool {
	name, _, assignment := strings.Cut(word, "=")
	return assignment && ValidShellEnvName(name)
}
