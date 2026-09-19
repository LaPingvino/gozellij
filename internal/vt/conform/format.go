package conform

import (
	"fmt"
	"strconv"
	"strings"
)

// The corpus is two plain files per case, because a corpus you cannot read in a diff is one nobody
// reviews:
//
//   - <name>.in    what to write to the terminal, with \e for escape. Lines beginning with # are
//                  comments and a newline is never implied - if a case wants one it writes \n.
//   - <name>.want  the screen the oracle recorded, as `cols`, `rows`, `cursor` and one `row` line
//                  per screen row.
//
// Neither is generated code. The .want file is recorded by tmux and checked in, so an emulator can
// be tested against it on a machine with no tmux at all.

// Unescape decodes a .in file into the bytes to write.
//
// Only the escapes a terminal case actually needs: \e, \n, \r, \t, \0, \\ and \xNN. An unknown
// escape is an error rather than a passthrough, because a typo like \E silently becoming "E" would
// make a case test something other than what it says.
func Unescape(s string) ([]byte, error) {
	var out []byte
	for _, raw := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		for i := 0; i < len(raw); i++ {
			c := raw[i]
			if c != '\\' {
				out = append(out, c)
				continue
			}
			i++
			if i >= len(raw) {
				return nil, fmt.Errorf("a backslash at the end of a line escapes nothing")
			}
			switch raw[i] {
			case 'e':
				out = append(out, 0x1b)
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case '0':
				out = append(out, 0)
			case '\\':
				out = append(out, '\\')
			case 'x':
				if i+2 >= len(raw) {
					return nil, fmt.Errorf("\\x needs two hex digits")
				}
				v, err := strconv.ParseUint(raw[i+1:i+3], 16, 8)
				if err != nil {
					return nil, fmt.Errorf("\\x%s is not two hex digits", raw[i+1:i+3])
				}
				out = append(out, byte(v))
				i += 2
			default:
				return nil, fmt.Errorf("unknown escape \\%c", raw[i])
			}
		}
	}
	return out, nil
}

// String writes a screen in the .want format.
func (s Screen) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cols %d\nrows %d\ncursor %d %d\n", s.Cols, s.Rows, s.CursorRow, s.CursorCol)
	for _, l := range s.Lines {
		l = escapeRow(l)
		if l == "" {
			b.WriteString("row\n")
			continue
		}
		fmt.Fprintf(&b, "row %s\n", l)
	}
	return b.String()
}

// ParseScreen reads the .want format back.
func ParseScreen(text string) (Screen, error) {
	var s Screen
	var sawCols, sawRows, sawCursor bool
	for n, raw := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if raw == "" {
			continue
		}
		key, rest, _ := strings.Cut(raw, " ")
		var err error
		switch key {
		case "cols":
			s.Cols, err = strconv.Atoi(strings.TrimSpace(rest))
			sawCols = true
		case "rows":
			s.Rows, err = strconv.Atoi(strings.TrimSpace(rest))
			sawRows = true
		case "cursor":
			row, col, ok := strings.Cut(strings.TrimSpace(rest), " ")
			if !ok {
				err = fmt.Errorf("cursor takes a row and a column")
				break
			}
			if s.CursorRow, err = strconv.Atoi(row); err == nil {
				s.CursorCol, err = strconv.Atoi(col)
			}
			sawCursor = true
		case "row":
			// Exactly one space after the keyword is the separator; anything beyond it is
			// content, so a row that genuinely starts with a space survives the round trip.
			var row string
			row, err = unescapeRow(rest)
			s.Lines = append(s.Lines, row)
		default:
			err = fmt.Errorf("unknown key %q", key)
		}
		if err != nil {
			return Screen{}, fmt.Errorf("line %d: %w", n+1, err)
		}
	}
	// A recording that is missing its header would otherwise compare as a 0x0 screen, which is a
	// difference report about the wrong thing entirely.
	if !sawCols || !sawRows || !sawCursor {
		return Screen{}, fmt.Errorf("a recording needs cols, rows and cursor")
	}
	return s, nil
}

// escapeRow makes a row safe to keep in a text file.
//
// A recording must be readable and diffable, and a stray control byte in one - a terminal reply
// that a capture echoed back, say - turns the file into something a pager mangles and a review
// skims past. Backslash and control characters go in as \xNN; everything printable, including
// every non-ASCII letter, stays exactly as it is, because the point of the file is being read.
func escapeRow(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString("\\\\")
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unescapeRow(s string) (string, error) {
	if !strings.Contains(s, "\\") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("a backslash at the end of a row escapes nothing")
		}
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 'x':
			if i+2 >= len(s) {
				return "", fmt.Errorf("\\x needs two hex digits")
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("\\x%s is not two hex digits", s[i+1:i+3])
			}
			b.WriteByte(byte(v))
			i += 2
		default:
			return "", fmt.Errorf("unknown escape \\%c in a row", s[i])
		}
	}
	return b.String(), nil
}
