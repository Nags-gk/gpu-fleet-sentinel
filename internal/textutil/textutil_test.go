package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is too long", 10, "this is..."},
		{"日本語のテキスト", 10, "日本..."}, // 3-byte runes: must not cut mid-rune
		{"abcdef", 3, "abc"},
		{"abcdef", 0, ""},
		{"é", 1, ""}, // found by fuzzing: limit smaller than the first rune
	} {
		if got := Truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func FuzzTruncate(f *testing.F) {
	f.Add("hello world", 8)
	f.Add("日本語のテキスト", 10)
	f.Add(strings.Repeat("é", 50), 7)
	f.Fuzz(func(t *testing.T, s string, n int) {
		n %= 2000
		out := Truncate(s, n)
		if n >= 0 && len(out) > n && len(s) > n {
			t.Fatalf("len %d exceeds limit %d", len(out), n)
		}
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("valid input produced invalid UTF-8: %q", out)
		}
		if len(s) <= n && out != s {
			t.Fatalf("input within limit was modified")
		}
	})
}
