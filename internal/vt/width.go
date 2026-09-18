package vt

import "unicode"

// StringWidth is how many terminal columns a string occupies.
//
// Not its length in bytes, and not its length in runes. A terminal lays text out in cells, and
// three different things map onto that badly: a multi-byte rune is one cell, a combining mark is
// *no* cell because it belongs to the one before it, and an East Asian wide character is two.
// Measuring with len() puts a status line's right-hand end in the wrong column for anyone whose
// hostname is not ASCII, and slicing with len() cuts a rune in half - which does not merely look
// wrong, it sends the terminal a byte sequence that is not a character at all.
//
// DESIGN.md names getting this wrong as the single most common way a multiplexer corrupts a
// screen, so it lives here next to Cell.Width rather than in whichever package first needed it:
// the emulator will want exactly this table.
func StringWidth(s string) int {
	w := 0
	for _, r := range s {
		w += RuneWidth(r)
	}
	return w
}

// RuneWidth is how many columns one rune occupies: 0 for a combining mark, 2 for an East Asian
// wide or fullwidth character, 1 otherwise.
//
// Control characters are counted as zero rather than as something: a status line should never
// contain one, and if it somehow does, the honest answer is that it takes no space rather than a
// guess that puts everything after it in the wrong column.
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7f && r < 0xa0):
		return 0
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		// Combining marks and format characters belong to the cell before them.
		return 0
	case isWide(r):
		return 2
	default:
		return 1
	}
}

// TruncateToWidth cuts a string to at most w columns, never in the middle of a rune.
//
// It returns fewer columns than asked for rather than more: a wide character that would straddle
// the limit is dropped, because half of one is not a thing a terminal can draw.
func TruncateToWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	used := 0
	for i, r := range s {
		rw := RuneWidth(r)
		if used+rw > w {
			return s[:i]
		}
		used += rw
	}
	return s
}

// wideRanges are the East Asian Wide and Fullwidth blocks, from Unicode's EastAsianWidth table.
//
// A table rather than a dependency. It is the stable, well-known part of the problem - these
// blocks have not moved in years - and it keeps a program that is deliberately thin on
// dependencies from taking one for fifteen lines of data.
var wideRanges = []struct{ lo, hi rune }{
	{0x1100, 0x115F},   // Hangul Jamo initial consonants
	{0x2E80, 0x303E},   // CJK radicals, Kangxi, CJK symbols
	{0x3041, 0x33FF},   // Hiragana, Katakana, Bopomofo, Hangul compat, CJK compat
	{0x3400, 0x4DBF},   // CJK extension A
	{0x4E00, 0x9FFF},   // CJK unified ideographs
	{0xA000, 0xA4CF},   // Yi
	{0xAC00, 0xD7A3},   // Hangul syllables
	{0xF900, 0xFAFF},   // CJK compatibility ideographs
	{0xFE10, 0xFE19},   // vertical forms
	{0xFE30, 0xFE6F},   // CJK compatibility forms
	{0xFF00, 0xFF60},   // fullwidth forms
	{0xFFE0, 0xFFE6},   // fullwidth signs
	{0x1F300, 0x1F64F}, // emoji: symbols and pictographs, emoticons
	{0x1F900, 0x1F9FF}, // supplemental symbols and pictographs
	{0x20000, 0x2FFFD}, // CJK extension B and beyond
	{0x30000, 0x3FFFD},
}

func isWide(r rune) bool {
	// Linear over sixteen ranges: this is called per rune of a status line, not per cell of a
	// screen. When the emulator needs it per cell, binary search it.
	for _, rg := range wideRanges {
		if r >= rg.lo && r <= rg.hi {
			return true
		}
	}
	return false
}
