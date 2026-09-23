package grid

import "testing"

// A control string that is consumed is not a gap, and one that asks a question is.
//
// vim opens every session with "\x1bPzz\x1b\\" followed by a cursor-position request: it is
// finding out whether the terminal swallows a control string or prints its body. Swallowing it
// is correct, so reporting it as unimplemented put a fault on the status line of every pane
// running an editor - which is how this was found, by an acceptance check that read row 14 of a
// split and got a complaint instead of a status line.
func TestControlStringsReportOnlyWhatWentUnanswered(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string // the key noteUnknown should record, or "" for silence
	}{
		{"vim's probe", "\x1bPzz\x1b\\", ""},
		{"an empty DCS", "\x1bP\x1b\\", ""},
		{"a privacy message", "\x1b^anything\x1b\\", ""},
		{"a kitty image", "\x1b_Ga=T,f=100;AAAA\x1b\\", "APC G image"},
		{"an application command that is not one", "\x1b_Xwhatever\x1b\\", ""},
		{"sixel with parameters", "\x1bP0;0;0q#0;2;0;0;0#0~~\x1b\\", "DCS sixel image"},
		{"sixel with none", "\x1bPq#0~~\x1b\\", "DCS sixel image"},
		{"a long body that is not sixel", "\x1bP1234567890123456789zz\x1b\\", ""},
		{"a start of string", "\x1bXwhatever\x1b\\", ""},
		{"DECRQSS, unanswered", "\x1bP$qm\x1b\\", "DCS $q request"},
		{"XTGETTCAP, unanswered", "\x1bP+q544e\x1b\\", "DCS +q request"},
		{"a DCS that states rather than asks", "\x1bP$rm\x1b\\", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			term := New(20, 4)
			term.Write([]byte(c.in))
			got := term.Unknown()
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("consumed correctly but reported %v", got)
				}
				return
			}
			if got[c.want] == 0 {
				t.Fatalf("want %q reported, got %v", c.want, got)
			}
		})
	}
}

// Split across writes, because a pty hands over whatever happened to be in the buffer and the
// decision is made from the first two body bytes - which is exactly the part that can arrive
// alone. The probe and the query have to survive being cut anywhere.
func TestControlStringSplitAcrossWrites(t *testing.T) {
	for _, in := range []string{
		"\x1bPzz\x1b\\",
		"\x1bP$qm\x1b\\",
		"\x1bP0;0;0q#0~~\x1b\\",
		"\x1b_Ga=T,f=100;AAAA\x1b\\",
	} {
		for cut := 1; cut < len(in); cut++ {
			whole := New(20, 4)
			whole.Write([]byte(in))
			piece := New(20, 4)
			piece.Write([]byte(in[:cut]))
			piece.Write([]byte(in[cut:]))
			if len(whole.Unknown()) != len(piece.Unknown()) {
				t.Fatalf("%q cut at %d: whole reported %v, split reported %v",
					in, cut, whole.Unknown(), piece.Unknown())
			}
		}
	}
}
