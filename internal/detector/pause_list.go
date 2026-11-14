package detector

import (
	"fmt"
	"strings"
)

// The pause list is the user's choice of apps that must stop folding while
// they run, whatever their GPU use. Patterns use the ignore-list syntax:
// case-insensitive exact names, or a prefix with a trailing "*", matched
// against /proc/<pid>/comm and basename(argv[0]).

const (
	maxPausePatterns   = 64
	maxPausePatternLen = 64
	// maxPauseMatches caps the names reported per snapshot.
	maxPauseMatches = 16
)

// foldingProcesses are never matched by the pause list: pausing folding
// because the FAH core runs would stop folding forever.
var foldingProcesses = []string{"FahCore_*", "fahclient", "fah-client", "foldingathome"}

// ParsePauseProcesses validates a comma-separated pause list, for example
// "blender,obs,resolve*". It trims and lowercases each pattern, drops empty
// and duplicate entries, and rejects patterns that would match everything or
// the Folding@home processes themselves.
func ParsePauseProcesses(list string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(list, ",") {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" || seen[p] {
			continue
		}
		switch {
		case len(p) > maxPausePatternLen:
			return nil, fmt.Errorf("detector: pause pattern %q longer than %d characters", p, maxPausePatternLen)
		case strings.TrimRight(p, "*") == "":
			return nil, fmt.Errorf("detector: pause pattern %q matches every process", p)
		case strings.ContainsAny(p, "/\\\x00"):
			return nil, fmt.Errorf("detector: pause pattern %q must be a process name, not a path", p)
		case strings.Contains(strings.TrimSuffix(p, "*"), "*"):
			return nil, fmt.Errorf("detector: pause pattern %q: \"*\" is only allowed at the end", p)
		}
		for _, f := range foldingProcesses {
			if matchPattern([]string{p}, strings.TrimSuffix(f, "*")) || matchPattern([]string{f}, strings.TrimSuffix(p, "*")) {
				return nil, fmt.Errorf("detector: pause pattern %q matches the Folding@home process %q", p, f)
			}
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) > maxPausePatterns {
		return nil, fmt.Errorf("detector: %d pause patterns, at most %d", len(out), maxPausePatterns)
	}
	return out, nil
}

// matchPattern applies the case-insensitive exact/prefix rules of the ignore
// and pause lists.
func matchPattern(patterns []string, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, pattern := range patterns {
		p := strings.ToLower(pattern)
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if name == p {
			return true
		}
	}
	return false
}
