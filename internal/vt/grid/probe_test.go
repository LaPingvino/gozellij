package grid

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestProbeRealPrograms is a survey, not a promise.
//
// It runs real programs on a real pty, feeds their output to this emulator, and reports what each
// of them sent that the emulator does not implement. A survey rather than a check because the
// answer depends on which programs are installed and which versions, so a failure would be a fact
// about the machine.
//
// It is how the list this emulator worked through was chosen, instead of implementing whatever
// looked important: eleven programs sent five things between them, and the two that mattered were
// not the two I would have guessed. It found that a DCS body was being drawn on the screen - vim
// puts "$qm" in the corner - and that every interactive program sets DECCKM, which was being
// dropped silently by the one branch of the parser that had no default and so could not be
// surveyed at all.
//
// Skipped by default, and never in `make check`. Run it by hand:
//
//	GOZELLIJ_PROBE=1 go test ./internal/vt/grid -run Probe -v
//
// What was left when this was written: OSC 8 hyperlinks (man), OSC 10 and 11 colour queries
// (vim), and DCS bodies, which are read to their end and thrown away on purpose. All three were
// then dealt with, and the eleven went quiet - at which point the survey had stopped telling
// anybody anything, so a second round of programs was added to make it ask something new.
//
// It did, and from one program. Of the twelve added, eleven were silent and fish sent four things
// nothing else here sends:
//
//	OSC 133      semantic prompt marks: where a prompt begins, where the command starts, where
//	             its output ends. Positional, so it belongs to the screen it was sent for - in a
//	             pane it would tell the outer terminal about rows that are not where it thinks.
//	OSC 7        the working directory. State, latest wins, and the one a terminal actually uses
//	             for something a person notices: a new tab opens where the old one was.
//	CSI ?2031    ask to be told when the colour scheme changes. A request for notifications this
//	             terminal cannot promise to send.
//
// nvim sends DCS +q (XTGETTCAP), which vim does not - so that question is asked in the wild, and
// goes unanswered here as it does under tmux.
func TestProbeRealPrograms(t *testing.T) {
	if os.Getenv("GOZELLIJ_PROBE") == "" {
		t.Skip("set GOZELLIJ_PROBE=1 to run the survey; it needs real programs and a real pty")
	}
	cases := []struct {
		name string
		args []string
		keys string
	}{
		{"bash", []string{"bash", "-i"}, "ls --color=always\nexport PS1='x$ '\n"},
		{"vim", []string{"vim", "-u", "NONE", "-c", "set nocompatible ruler", "/etc/hostname"}, "jjix\x1b:q!\n"},
		{"nvim", []string{"nvim", "--clean", "/etc/hostname"}, "jjix\x1b:q!\n"},
		{"less", []string{"less", "/etc/services"}, " bq"},
		{"htop", []string{"htop"}, "q"},
		{"top", []string{"top"}, "q"},
		{"man", []string{"man", "ls"}, " q"},
		{"git", []string{"git", "log", "--color=always", "--graph", "--oneline", "-20"}, "q"},
		{"python3", []string{"python3"}, "1+1\nexit()\n"},
		{"nano", []string{"nano", "/etc/hostname"}, "\x18n"},
		{"mc", []string{"mc"}, "\x1b0"},

		// A second round, added once the first eleven had all gone quiet. A survey that reports
		// nothing has stopped surveying, and the way to make it useful again is to ask programs
		// that are not like the ones already asked - a different editor lineage, a shell that is
		// not bash, a full-screen program that is not curses, and a multiplexer, which is the
		// one kind of program whose whole job is to speak this language back at you.
		{"emacs", []string{"emacs", "-nw", "-Q", "/etc/hostname"}, "\x18\x03"},
		{"helix", []string{"hx", "/etc/hostname"}, "jji\x1b:q!\n"},
		{"w3m", []string{"w3m", "/etc/services"}, " q y"},
		{"weechat", []string{"weechat"}, "/quit\n"},
		{"whiptail", []string{"whiptail", "--msgbox", "probing", "10", "40"}, "\n"},
		{"screen", []string{"screen", "-D", "-m", "bash", "-i"}, "ls --color=always\nexit\n"},
		{"fish", []string{"fish", "-i"}, "ls --color=always\nexit\n"},
		{"zsh", []string{"zsh", "-i"}, "ls --color=always\nexit\n"},
		{"gdb", []string{"gdb", "-q"}, "quit\n"},
		{"sqlite3", []string{"sqlite3"}, "select 1;\n.quit\n"},
		{"node", []string{"node", "-i"}, "1+1\n.exit\n"},
		{"nethack", []string{"nethack"}, "\x1b\x1bqy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(c.args[0], c.args[1:]...)
			cmd.Env = append(os.Environ(), "TERM=xterm-256color", "LANG=en_US.UTF-8", "COLORTERM=truecolor")
			f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
			if err != nil {
				t.Skipf("%s: %v", c.name, err)
			}
			term := New(80, 24)
			done := make(chan struct{})
			go func() {
				defer close(done)
				buf := make([]byte, 4096)
				for {
					n, err := f.Read(buf)
					if n > 0 {
						_, _ = term.Write(buf[:n])
						if r := term.TakeReplies(); len(r) > 0 {
							_, _ = f.Write(r)
						}
					}
					if err != nil {
						return
					}
				}
			}()
			time.Sleep(700 * time.Millisecond)
			_, _ = io.WriteString(f, c.keys)
			time.Sleep(900 * time.Millisecond)
			_ = f.Close()
			_ = cmd.Process.Kill()
			<-done
			_ = cmd.Wait()
			if u := term.Unknown(); len(u) > 0 {
				for name, n := range u {
					t.Logf("%-8s %-18s x%d", c.name, name, n)
				}
			} else {
				t.Logf("%-8s everything it sent is implemented", c.name)
			}
		})
	}
}
