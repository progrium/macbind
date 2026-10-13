// Package shquote quotes strings for POSIX shells. Its output is also what
// the server's ctl file parser (wanix's shlex) expects.
package shquote

import "strings"

// Quote returns s unchanged if it only contains shell-safe characters,
// otherwise wrapped in single quotes with embedded single quotes escaped.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, unsafe) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func unsafe(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+,:@%", r))
}
