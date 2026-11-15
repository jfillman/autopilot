package auth

// FnmatchCase is Python's fnmatch.fnmatchcase (the rules the allow-lists use), for the lists of other packages that are
// written for the same rules (profile.glidepath.apps, matched by the core's glob.any).
func FnmatchCase(name, pattern string) bool { return fnmatchCase(name, pattern) }
