// Package textutil holds small string helpers shared by the agent, controller
// and incident summarizer.
package textutil

import "unicode/utf8"

// Truncate shortens s to at most n bytes, ending in "..." when it cut
// something. It never splits a multi-byte character, so valid UTF-8 in means
// valid UTF-8 out. Messages end up in node conditions and annotations, which
// are serialized as JSON; a split rune would be silently rewritten there.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	suffix := "..."
	cut := n - len(suffix)
	if n <= len(suffix) { // no room for the ellipsis
		suffix, cut = "", n
	}
	cut = max(cut, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
