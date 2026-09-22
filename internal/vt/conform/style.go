package conform

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// Styles are compared as a short signature per display column: "b" for bold, "u" for underline,
// "31" for an indexed foreground, and so on, with "-" for a cell in the terminal's default.
//
// A signature rather than a struct so that a recording stays a text file a person can read and a
// disagreement names something recognisable. The set is what tmux reports and what this emulator
// stores; anything outside it is not compared, and the harness says so rather than implying that a
// passing case blesses the whole of SGR.
//
// The parser below is deliberately its own, not the emulator's. Interpreting the oracle's output
// with the code under test would make a wrong SGR implementation agree with itself perfectly - the
// exact circularity a differential harness exists to avoid. It handles only SGR and text, because
// that is all capture-pane -e emits.

// styleSig is the signature of one cell's style.
func styleSig(s vt.Style) string {
	var parts []string
	if s.Bold {
		parts = append(parts, "b")
	}
	if s.Faint {
		parts = append(parts, "f")
	}
	if s.Italic {
		parts = append(parts, "i")
	}
	if s.Underline {
		parts = append(parts, "u")
	}
	if s.Blink {
		parts = append(parts, "k")
	}
	if s.Reverse {
		parts = append(parts, "r")
	}
	if s.Strikethrough {
		parts = append(parts, "s")
	}
	if c := colorSig(s.Fg, false); c != "" {
		parts = append(parts, c)
	}
	if c := colorSig(s.Bg, true); c != "" {
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "")
}

func colorSig(c vt.Color, bg bool) string {
	prefix := ""
	if bg {
		prefix = "^"
	}
	switch c.Kind {
	case vt.ColorIndexed:
		return fmt.Sprintf("%s%d", prefix, c.Index)
	case vt.ColorRGB:
		return fmt.Sprintf("%s#%02x%02x%02x", prefix, c.R, c.G, c.B)
	default:
		return ""
	}
}

// styledScreen turns the whole of `capture-pane -e` into a signature per display column per row.
//
// The whole thing at once, because the style carries from one row to the next: tmux emits the
// escape sequences needed to reproduce the screen, so an attribute that is still in force is not
// repeated. vim colours the tildes past the end of a buffer by emitting one \e[94m before the
// first of them, and parsing row by row gave the first tilde a colour and the eleven below it
// none. The recording said so plainly and the emulator was blamed for it first.
func styledScreen(lines []string) [][]string {
	var (
		out   [][]string
		style vt.Style
	)
	for _, line := range lines {
		out = append(out, trimDefaults(styledColumns(line, &style)))
	}
	return out
}

// styledColumns turns one row into signatures, continuing from and updating the running style.
func styledColumns(line string, carried *vt.Style) []string {
	var out []string
	style := *carried
	defer func() { *carried = style }()
	for i := 0; i < len(line); i++ {
		if line[i] == '\t' {
			// A captured tab stands for the cells the cursor skipped, and they are cells: eight
			// of them, in whatever style is in force at this point in the capture. Counting it as
			// one column shifted every style after it seven columns left, and the emulator was
			// blamed for the difference.
			for next := (len(out)/8 + 1) * 8; len(out) < next; {
				out = append(out, styleSig(style))
			}
			continue
		}
		if line[i] != 0x1b {
			r, size := decodeRune(line[i:])
			w := vt.RuneWidth(r)
			switch {
			case w == 0 && len(out) > 0:
				// A combining mark belongs to the cell before it and carries no style of its own.
			case w == 2:
				out = append(out, styleSig(style), styleSig(style))
			default:
				out = append(out, styleSig(style))
			}
			i += size - 1
			continue
		}
		// An escape sequence. Only SGR (CSI ... m) changes anything here; anything else is
		// consumed and ignored, because capture-pane does not emit cursor movement.
		end := i + 1
		if end < len(line) && line[end] == '[' {
			end++
			for end < len(line) && (line[end] >= 0x30 && line[end] <= 0x3f) {
				end++
			}
			if end < len(line) && line[end] == 'm' {
				applySGR(&style, line[i+2:end])
			}
		}
		for end < len(line) && (line[end] < 0x40 || line[end] > 0x7e) {
			end++
		}
		i = end
	}
	return out
}

func decodeRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 1
}

// applySGR is the independent half of the oracle: what the escape codes mean, written from the
// specification rather than shared with the emulator.
func applySGR(style *vt.Style, params string) {
	if params == "" {
		params = "0"
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 0:
			*style = vt.Style{}
		case n == 1:
			style.Bold = true
		case n == 2:
			style.Faint = true
		case n == 3:
			style.Italic = true
		case n == 4:
			style.Underline = true
		case n == 5:
			style.Blink = true
		case n == 7:
			style.Reverse = true
		case n == 9:
			style.Strikethrough = true
		case n == 22:
			style.Bold, style.Faint = false, false
		case n == 23:
			style.Italic = false
		case n == 24:
			style.Underline = false
		case n == 25:
			style.Blink = false
		case n == 27:
			style.Reverse = false
		case n == 29:
			style.Strikethrough = false
		case n >= 30 && n <= 37:
			style.Fg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 30)}
		case n >= 90 && n <= 97:
			style.Fg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 90 + 8)}
		case n == 39:
			style.Fg = vt.Color{}
		case n >= 40 && n <= 47:
			style.Bg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 40)}
		case n >= 100 && n <= 107:
			style.Bg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 100 + 8)}
		case n == 49:
			style.Bg = vt.Color{}
		case n == 38 || n == 48:
			c, used := extendedColor(fields[i+1:])
			if n == 38 {
				style.Fg = c
			} else {
				style.Bg = c
			}
			i += used
		}
	}
}

// extendedColor reads the 5;N and 2;R;G;B forms.
func extendedColor(rest []string) (vt.Color, int) {
	if len(rest) == 0 {
		return vt.Color{}, 0
	}
	switch rest[0] {
	case "5":
		if len(rest) < 2 {
			return vt.Color{}, 1
		}
		n, _ := strconv.Atoi(rest[1])
		return vt.Color{Kind: vt.ColorIndexed, Index: uint8(n)}, 2
	case "2":
		if len(rest) < 4 {
			return vt.Color{}, len(rest)
		}
		r, _ := strconv.Atoi(rest[1])
		g, _ := strconv.Atoi(rest[2])
		b, _ := strconv.Atoi(rest[3])
		return vt.Color{Kind: vt.ColorRGB, R: uint8(r), G: uint8(g), B: uint8(b)}, 4
	}
	return vt.Color{}, 1
}
