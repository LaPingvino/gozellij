package grid

// The DEC special graphics character set: the one that draws boxes.
//
// A program selects it with ESC ( 0 and goes back with ESC ( B, or switches between two selected
// sets with SI and SO. While it is in force, "lqqqk" is not text, it is the top of a box. ncurses
// uses it whenever the terminal description says to, which is most of the time on a serial-ish
// TERM, so ignoring it shows a screen of scattered letters where a person expects lines.
//
// Translated at the point the character is stored, to the Unicode the glyph means. The alternative
// - keeping the letter and a flag saying how to draw it - is what a terminal does internally and
// needs the flag carried through every cell, every copy and every renderer. This way the grid holds
// what is on the screen, which is the question anything asking a grid is actually asking.
//
// One consequence, written here because it is not obvious: a recording from tmux reports the
// letter rather than the glyph, because capture-pane gives the underlying character. The corpus
// therefore cannot tell a terminal that applies this from one that ignores it, and the check for
// it is in scripts/acceptance.sh, which looks at what a real screen ends up showing.
var decSpecialGraphics = map[byte]string{
	'`': "◆", 'a': "▒", 'b': "␉", 'c': "␌", 'd': "␍", 'e': "␊", 'f': "°", 'g': "±",
	'h': "␤", 'i': "␋", 'j': "┘", 'k': "┐", 'l': "┌", 'm': "└", 'n': "┼", 'o': "⎺",
	'p': "⎻", 'q': "─", 'r': "⎼", 's': "⎽", 't': "├", 'u': "┤", 'v': "┴", 'w': "┬",
	'x': "│", 'y': "≤", 'z': "≥", '{': "π", '|': "≠", '}': "£", '~': "·",
}

// charset is which set a slot holds. Only two matter: ordinary text, and the one that draws boxes.
type charset int

const (
	charsetASCII charset = iota
	charsetGraphics
)

// selectCharset handles ESC ( <c> and ESC ) <c>: what G0 and G1 hold.
func (t *Term) selectCharset(slot int, code byte) {
	set := charsetASCII
	if code == '0' {
		set = charsetGraphics
	}
	if slot == 0 {
		t.g0 = set
		return
	}
	t.g1 = set
}

// shiftTo handles SI and SO: which of G0 and G1 is in use.
func (t *Term) shiftTo(slot int) { t.active = slot }

// mapRune translates a character through the current set, when there is anything to translate.
func (t *Term) mapRune(r rune) (string, bool) {
	set := t.g0
	if t.active == 1 {
		set = t.g1
	}
	if set != charsetGraphics || r > 0x7f {
		return "", false
	}
	glyph, ok := decSpecialGraphics[byte(r)]
	return glyph, ok
}
