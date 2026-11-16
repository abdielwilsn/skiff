// Package shell has small helpers for building bash scripts safely.
package shell

import "strings"

// Quote returns s as a single-quoted bash word, safe to splice into a script.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
