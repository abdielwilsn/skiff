package web

import (
	"regexp"
	"strings"
)

var (
	// Lowercase hostnames or IPv4 addresses. No slashes, spaces or "..", so a
	// domain is always safe as a directory and file name.
	domainRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	branchRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)
	clientIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	emailRe    = regexp.MustCompile(`^[^\s@'"]{1,64}@[A-Za-z0-9.-]{1,190}\.[A-Za-z]{2,}$`)
)

func validDomain(d string) bool { return len(d) <= 253 && domainRe.MatchString(d) }

func validRepo(r string) bool {
	if !repoRe.MatchString(r) {
		return false
	}
	name := r[strings.IndexByte(r, '/')+1:]
	return name != "." && name != ".."
}

func validBranch(b string) bool {
	return branchRe.MatchString(b) &&
		!strings.Contains(b, "..") && !strings.Contains(b, "//") &&
		!strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".lock")
}

// normalizeRepo accepts "owner/name" or a GitHub URL and returns "owner/name".
func normalizeRepo(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "github.com/"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(s, "/")
	return strings.TrimSuffix(s, ".git")
}

func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n") // browsers submit textareas with CRLF; bash chokes on \r
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}
