package auth

import "github.com/jfillman/autopilot/gateway/internal/glob"

// fnmatchCase and matchAny are the Python gateway's fnmatch rules (internal/glob).
func fnmatchCase(name, pattern string) bool { return glob.Match(name, pattern) }

func matchAny(name string, globs []string) bool { return glob.MatchAny(name, globs) }
