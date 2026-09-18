package vt

import "testing"

func TestRuneWidthCountsCellsNotBytes(t *testing.T) {
	cases := map[rune]int{
		'a':  1,
		'é':  1, // one rune, one cell, two bytes
		'服':  2, // East Asian wide
		'Ａ':  2, // fullwidth
		'́':  0, // combining acute: belongs to the cell before it
		'​':  0, // zero-width space (format character)
		0:    0,
		'\n': 0,
	}
	for r, want := range cases {
		if got := RuneWidth(r); got != want {
			t.Errorf("RuneWidth(%q) = %d, want %d", r, got, want)
		}
	}
}

func TestStringWidthIsColumnsNotLength(t *testing.T) {
	cases := map[string]int{
		"":       0,
		"abc":    3,
		"café":   4,  // 5 bytes
		"é":     1,  // e + combining acute is one cell
		"服务器":    6,  // three wide characters
		"[服务器一]": 10, // brackets are one each, four wide characters
	}
	for s, want := range cases {
		if got := StringWidth(s); got != want {
			t.Errorf("StringWidth(%q) = %d, want %d (len is %d)", s, got, want, len(s))
		}
	}
}

func TestTruncateNeverCutsARuneInHalf(t *testing.T) {
	// The reason this matters: half a rune is not a character, it is a byte sequence the terminal
	// cannot render and may misinterpret.
	if got := TruncateToWidth("café", 3); got != "caf" {
		t.Errorf("TruncateToWidth(café, 3) = %q, want caf", got)
	}
	for _, w := range []int{1, 2, 3, 4, 5, 6} {
		got := TruncateToWidth("服务器", w)
		if StringWidth(got) > w {
			t.Errorf("TruncateToWidth(服务器, %d) = %q, which is %d columns", w, got, StringWidth(got))
		}
		// Every byte kept must form whole runes.
		for i, r := range got {
			if r == '�' && len(got)-i < 3 {
				t.Errorf("TruncateToWidth(服务器, %d) = %q, which ends mid-rune", w, got)
			}
		}
	}
	// A wide character that would straddle the limit is dropped rather than half-drawn.
	if got := TruncateToWidth("服务器", 3); StringWidth(got) != 2 {
		t.Errorf("TruncateToWidth(服务器, 3) = %q (%d columns), want the two-column prefix", got, StringWidth(got))
	}
	if got := TruncateToWidth("abc", 0); got != "" {
		t.Errorf("TruncateToWidth(abc, 0) = %q, want empty", got)
	}
}
