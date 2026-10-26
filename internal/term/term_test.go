package term

import "testing"

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"plain":               "plain",
		"\x1b[31mred\x1b[0m":  "[31mred[0m",
		"a\rb\nc\td":          "abcd",
		"\x1b]0;title\x07":    "]0;title",
		"\u0085line\u009bcsi": "linecsi",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeText("a\r\nb\tc\x1b[2J"); got != "a\nb\tc[2J" {
		t.Errorf("SanitizeText = %q", got)
	}
}
