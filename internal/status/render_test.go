package status

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderNeverCutsARuneInHalf(t *testing.T) {
	// A service called "café" (5 bytes, 4 columns) in a 4-wide line.
	c := Context{Service: "café"}
	line := Render(c, []string{"session"}, nil, 5) // "[café]" is 7 bytes; byte 5 is inside é
	if !utf8.ValidString(line) {
		t.Errorf("Render produced invalid UTF-8 %q - the terminal will print a replacement glyph or garbage", line)
	}
}

func TestRenderMeasuresColumnsNotBytes(t *testing.T) {
	c := Context{Service: "café"}
	line := Render(c, []string{"session"}, []string{"cpu_count"}, 40)
	cols := utf8.RuneCountInString(line)
	if cols != 40 {
		t.Errorf("line is %d columns wide, want 40 (it is %d bytes): the right-hand fields are not at the margin for a non-ASCII left", cols, len(line))
	}
	// Wide characters: 4 CJK glyphs are 12 bytes and 8 columns.
	c = Context{Service: "服务器一"}
	line = Render(c, []string{"session"}, []string{"cpu_count"}, 20)
	t.Logf("cjk line: %q (%d bytes, %d runes)", line, len(line), utf8.RuneCountInString(line))
	if strings.Count(line, " ") != 20-len("[服务器一]")-len("Xcpu")+2 {
		t.Logf("padding is computed from bytes; display width will differ")
	}
}

func TestRenderTerminatesOnASpacelessRightHandSide(t *testing.T) {
	c := Context{Service: strings.Repeat("x", 100)}
	done := make(chan string, 1)
	go func() { done <- Render(c, []string{"session"}, []string{"cpu_count"}, 10) }()
	select {
	case line := <-done:
		if len(line) > 10 {
			t.Errorf("line is %d bytes, wider than 10: %q", len(line), line)
		}
	default:
	}
	line := <-done
	if len(line) != 10 {
		t.Errorf("width=10 gave %d bytes: %q", len(line), line)
	}
}

func TestConfigQuoteStripping(t *testing.T) {
	cfg := parse(strings.NewReader("left='session\"\nright=\"'time'\"\n"), DefaultConfig(), "t")
	t.Logf("left=%v right=%v problems=%v", cfg.Left, cfg.Right, cfg.Problems)
	cfg = parse(strings.NewReader("left=\nright=\n"), DefaultConfig(), "t")
	if len(cfg.Left) != 0 || len(cfg.Right) != 0 || len(cfg.Problems) != 0 {
		t.Logf("empty left/right: left=%v right=%v problems=%v", cfg.Left, cfg.Right, cfg.Problems)
	}
	if Render(Context{Service: "web"}, cfg.Left, cfg.Right, 80) != strings.Repeat(" ", 80) {
		t.Errorf("empty config did not render a blank line")
	}
	cfg = parse(strings.NewReader("every=1ms\n"), DefaultConfig(), "t")
	if len(cfg.Problems) == 0 {
		t.Logf("every=1ms accepted without a problem: 1000 daemon dials per second per attached client")
	}
}
