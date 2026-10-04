package logfmt

import "testing"

func TestTruncateCutsAtARuneBoundary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short enough is untouched", "abc", 10, "abc"},
		{"exact fit is untouched", "abc", 3, "abc"},
		{"ascii is cut exactly", "abcdef", 3, "abc"},
		{"non-positive n yields nothing", "abc", 0, ""},
		{"negative n yields nothing", "abc", -5, ""},
		{"a multi-byte rune is never split", "中文错误信息", 5, "中"},
		{"landing on a boundary keeps the rune", "中文错误信息", 6, "中文"},
		{"surrounding space is trimmed", "  abc  ", 10, "abc"},
		{"trimming can empty the value", "   ", 10, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.in, tc.n); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestUID8IsTheCanonicalShortForm(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "-"},
		{"   ", "-"},
		{"abc", "abc"},
		{"12345678", "12345678"},
		{"123456789", "12345678"},
		{"uid-cn-0001", "uid-cn-0"},
	}
	for _, tc := range cases {
		if got := UID8(tc.in); got != tc.want {
			t.Errorf("UID8(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLabelKeepsTheIDNextToTheNickName(t *testing.T) {
	cases := []struct {
		uid, nick, want string
	}{
		{"", "", "-"},
		{"1234567890", "", "12345678"},
		{"1234567890", "work", "work(12345678)"},
		{"1234567890", "  工作  ", "工作(12345678)"},
	}
	for _, tc := range cases {
		if got := Label(tc.uid, tc.nick); got != tc.want {
			t.Errorf("Label(%q, %q) = %q, want %q", tc.uid, tc.nick, got, tc.want)
		}
	}
}

func TestDisplayWidthCountsColumnsNotBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"中文", 4},
		{"工作号", 6},
		{"ｆｕｌｌ", 8},
		{"🎉", 2},
		{"\x01", 0},
		{"a中", 3},
	}
	for _, tc := range cases {
		if got := DisplayWidth(tc.in); got != tc.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPadOnlyEverAdds(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"abc", 6, "abc   "},
		{"abc", 3, "abc"},
		{"abc", 2, "abc"},
		{"abc", 0, "abc"},
		{"abc", -1, "abc"},
		{"中", 4, "中  "},
		{"工作号", 8, "工作号  "},
	}
	for _, tc := range cases {
		if got := Pad(tc.in, tc.width); got != tc.want {
			t.Errorf("Pad(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

// TestPadUsesDisplayWidthNotByteLength is the reason the width table exists: a
// byte-counted pad would leave a CJK value two columns short of its neighbours.
func TestPadUsesDisplayWidthNotByteLength(t *testing.T) {
	padded := Pad("工作号", 10)
	if got := DisplayWidth(padded); got != 10 {
		t.Fatalf("DisplayWidth(%q) = %d, want 10", padded, got)
	}
	if len(padded) != 13 {
		t.Fatalf("len(%q) = %d, want 13 bytes (9 for the CJK, 4 spaces)", padded, len(padded))
	}
}
