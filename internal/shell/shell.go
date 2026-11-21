// Package shell quotes words for POSIX shells.
package shell

import "strings"

// Quote returns s as one shell word, in single quotes.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Join returns args as a command line, quoting only the words that need it.
func Join(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.IndexFunc(a, func(r rune) bool { return !plain(r) }) < 0 {
			out[i] = a
		} else {
			out[i] = Quote(a)
		}
	}
	return strings.Join(out, " ")
}

func plain(r rune) bool {
	return r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' || r == ',' || r == '@' || r == '+' ||
		r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}
