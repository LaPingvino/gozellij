package status

import (
	"strings"
	"testing"
)

// A prefix key is configured in a file you cannot test without logging out, so the parser accepts
// the spellings people actually write and refuses the ones that would quietly break the keyboard.

func TestTheSpellingsPeopleWriteAllMeanTheSameKey(t *testing.T) {
	for _, s := range []string{"C-b", "c-b", "^B", "^b", "Ctrl-b", "CTRL-B", "ctrl+b", "0x02", "2"} {
		got, err := ParsePrefix(s)
		if err != nil {
			t.Errorf("ParsePrefix(%q): %v", s, err)
			continue
		}
		if got != 0x02 {
			t.Errorf("ParsePrefix(%q) = %#x, want 0x02 (Ctrl-B)", s, got)
		}
	}
}

func TestTheDefaultAndItsOddSpellingsRoundTrip(t *testing.T) {
	for _, c := range []struct {
		in   string
		want byte
	}{
		{"C-]", 0x1d}, {"^]", 0x1d}, {"0x1d", 0x1d},
		{"C-[", 0x1b}, {"C-\\", 0x1c}, {"C-_", 0x1f},
		{"C-a", 0x01}, {"C-z", 0x1a},
	} {
		got, err := ParsePrefix(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParsePrefix(%q) = %#x, %v; want %#x", c.in, got, err, c.want)
		}
		// And the label has to name it back, because every message about the key is built from
		// the label rather than spelling a default out.
		if lbl := PrefixLabel(c.want); lbl == "" {
			t.Errorf("PrefixLabel(%#x) is empty", c.want)
		}
	}
	if got := PrefixLabel(DefaultPrefix); got != "Ctrl-]" {
		t.Errorf("the default is labelled %q, want Ctrl-]", got)
	}
	if got := PrefixLabel(0x02); got != "Ctrl-B" {
		t.Errorf("0x02 is labelled %q, want Ctrl-B", got)
	}
}

func TestAKeyThatWouldBreakTheKeyboardIsRefused(t *testing.T) {
	// A number naming an ordinary character is the one to refuse. As a prefix it would mean every
	// press of that key is a command and never reaches the program, which from inside looks like a
	// broken terminal rather than like a setting.
	for _, s := range []string{"0x41", "65", "0x7f", "", "   ", "C-bb", "hello", "0x00"} {
		if got, err := ParsePrefix(s); err == nil {
			t.Errorf("ParsePrefix(%q) = %#x with no error; it should be refused", s, got)
		}
	}
}

func TestABareLetterMeansTheControlCharacter(t *testing.T) {
	// This test started life asserting that "b" was refused, which was a rule I had not thought
	// through. There is no other thing `prefix=b` could mean: the literal letter is refused a few
	// lines up, so reading it as Ctrl-B is the only useful interpretation, and refusing it would
	// be pedantry aimed at somebody who cannot test the file without logging out.
	for _, c := range []struct {
		in   string
		want byte
	}{{"b", 0x02}, {"B", 0x02}, {"a", 0x01}, {"z", 0x1a}} {
		got, err := ParsePrefix(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParsePrefix(%q) = %#x, %v; want %#x", c.in, got, err, c.want)
		}
	}
}

func TestAMisconfiguredPrefixIsReportedAndTheDefaultKept(t *testing.T) {
	// The failure this protects against: the file parses, the key does nothing, and there is no
	// way to tell from inside the session which of the two happened.
	cfg := parse(stringReader("prefix=nonsense\n"), DefaultConfig(), "test")
	if cfg.Prefix != DefaultPrefix {
		t.Errorf("a bad prefix changed the key to %#x", cfg.Prefix)
	}
	if len(cfg.Problems) != 1 {
		t.Fatalf("problems = %q, want exactly one", cfg.Problems)
	}
}

func TestAGoodPrefixIsTakenFromTheFile(t *testing.T) {
	cfg := parse(stringReader("prefix=C-b\n"), DefaultConfig(), "test")
	if cfg.Prefix != 0x02 {
		t.Errorf("prefix = %#x, want 0x02", cfg.Prefix)
	}
	if len(cfg.Problems) != 0 {
		t.Errorf("problems = %q, want none", cfg.Problems)
	}
}

func stringReader(s string) *strings.Reader { return strings.NewReader(s) }
