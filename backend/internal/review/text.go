package review

import "strings"

// OneLine collapses s onto a single line and truncates it to limit bytes,
// marking a cut with "...".
func OneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + "..."
	}
	return s
}
