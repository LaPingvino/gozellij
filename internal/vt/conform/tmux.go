package conform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// Record drives a case through a real tmux and returns what the screen became.
//
// tmux is the oracle because it is a terminal emulator that a great deal of software has been run
// under for two decades, and because the alternative - a second Go emulator library - would mean
// taking a dependency with its own open correctness bugs in order to define correctness. The cost
// is that recording needs tmux installed; comparing does not, because the recording is checked in.
//
// A private socket, always: recording must never touch a tmux the user is sitting in.
func Record(c Case) (Screen, error) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		// Loud, not skipped. A recorder that quietly does nothing when its oracle is absent
		// produces an empty corpus and a green test run, which is the exact shape of lie this
		// harness exists to prevent.
		return Screen{}, fmt.Errorf("tmux is needed to record a case and was not found: %w", err)
	}

	dir, err := os.MkdirTemp("", "conform")
	if err != nil {
		return Screen{}, err
	}
	defer os.RemoveAll(dir)

	sock := filepath.Join(dir, "sock")

	// A fixed configuration, never the user's. tmux reads ~/.tmux.conf by default, so a recording
	// made here would carry whatever that file says - and on the machine this corpus was first
	// recorded on it says `set -g history-limit 0`, which silently made every recording of
	// scrollback empty. A corpus that depends on whose laptop it was taken on is not an oracle.
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte(tmuxConfig), 0o600); err != nil {
		return Screen{}, err
	}

	// Each write step goes through its own file rather than the command line: a case is arbitrary
	// bytes, including NUL and escape, and quoting those into a shell command is a way to test the
	// quoting instead of the terminal.
	steps := c.Steps
	if len(steps) == 0 {
		steps = []Step{{Write: c.Input}}
	}
	var script strings.Builder
	// -onlcr as well as -echo: without it the pty turns every bare newline into a carriage return
	// and a newline before tmux sees it, so the oracle was answering a question about different
	// bytes than the ones the case contains. Every corpus case so far wrote \r\n explicitly, so
	// it never showed - until a generated stream contained a bare \n and the disagreement was
	// entirely the recorder's.
	script.WriteString("stty -echo -onlcr\n")
	writes := 0
	for i, st := range steps {
		if st.IsResize() {
			// The pane hands control back here and waits. A resize has to happen between two
			// writes, and nothing else orders the tmux socket against the pty.
			fmt.Fprintf(&script, "%s -S %s wait-for -S %s%d\n", tmux, sock, stepSignal, i)
			fmt.Fprintf(&script, "%s -S %s wait-for %s%d\n", tmux, sock, goSignal, i)
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("in%d", writes))
		if err := os.WriteFile(path, st.Write, 0o600); err != nil {
			return Screen{}, err
		}
		writes++
		fmt.Fprintf(&script, "cat %s\n", path)
	}
	fmt.Fprintf(&script, "%s -S %s wait-for -S %s\nsleep 300\n", tmux, sock, doneSignal)

	sh := filepath.Join(dir, "case.sh")
	if err := os.WriteFile(sh, []byte(script.String()), 0o700); err != nil {
		return Screen{}, err
	}

	tm := func(args ...string) (string, error) {
		out, err := exec.Command(tmux, append([]string{"-f", conf, "-S", sock}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out), nil
	}

	// The pane runs a generated script: write, hand back, wait, write again, and finally signal
	// that everything has been written. It must stay alive after that, because a pane that exits
	// takes the screen with it and there would be nothing to capture.
	//
	// The -echo is not tidiness. A real program's output contains device queries - vim asks for
	// the cursor position and the terminal's identity - and tmux answers them on the pty. With
	// echo on, the pane's own terminal discipline writes those answers back to the screen, and a
	// recording of vim came out with `^[[2;2R^[[3;1R^[[>84;0;0c` drawn across its first row. The
	// oracle was recording its own replies as if they were the program's output.
	if _, err := tm("new-session", "-d", "-x", strconv.Itoa(c.Cols), "-y", strconv.Itoa(c.Rows),
		"sh", sh); err != nil {
		return Screen{}, err
	}
	defer tm("kill-server")

	// Take the turns the script hands over: at each resize it stops and waits to be told to go on.
	for i, st := range steps {
		if !st.IsResize() {
			continue
		}
		if err := waitFor(tmux, sock, fmt.Sprintf("%s%d", stepSignal, i)); err != nil {
			return Screen{}, err
		}
		if _, err := tm("resize-window", "-x", strconv.Itoa(st.Cols), "-y", strconv.Itoa(st.Rows)); err != nil {
			return Screen{}, err
		}
		if _, err := tm("wait-for", "-S", fmt.Sprintf("%s%d", goSignal, i)); err != nil {
			return Screen{}, err
		}
	}

	if err := waitFor(tmux, sock, doneSignal); err != nil {
		return Screen{}, err
	}
	if err := settle(tm); err != nil {
		return Screen{}, err
	}

	content, err := tm("capture-pane", "-p")
	if err != nil {
		return Screen{}, err
	}
	// Everything above the visible screen: the user's scrollback, recorded because an emulator
	// that draws the screen correctly while losing what scrolled off is worse than the terminal it
	// replaced.
	//
	// The size is asked for first and the capture skipped when it is zero. `capture-pane -S - -E
	// -1` on a pane with no history does not return nothing - it returns the first *visible* line,
	// which quietly gave every case in the corpus one line of scrollback that was never there.
	var hist string
	size, err := tm("display", "-p", "#{history_size}")
	if err != nil {
		return Screen{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(size))
	if err != nil {
		return Screen{}, fmt.Errorf("tmux reported the history size as %q", size)
	}
	if n > 0 {
		if hist, err = tm("capture-pane", "-p", "-S", "-"+strconv.Itoa(n), "-E", "-1"); err != nil {
			return Screen{}, err
		}
	}
	// The same screen again with its escape sequences, which is where the styles come from. Two
	// captures rather than one parse of the styled form: the plain one is what a person reads in
	// the recording, and deriving it by stripping escapes would mean the harness's own stripper
	// stood between the oracle and the comparison.
	styled, err := tm("capture-pane", "-p", "-e")
	if err != nil {
		return Screen{}, err
	}

	pos, err := tm("display", "-p", "#{cursor_y} #{cursor_x} #{cursor_flag}")
	if err != nil {
		return Screen{}, err
	}
	fields := strings.Fields(pos)
	if len(fields) != 3 {
		return Screen{}, fmt.Errorf("tmux reported the cursor as %q", pos)
	}
	row, col := fields[0], fields[1]
	cols, rows := c.Cols, c.Rows
	for _, st := range c.Steps {
		if st.IsResize() {
			cols, rows = st.Cols, st.Rows
		}
	}
	s := Screen{Cols: cols, Rows: rows}
	if s.CursorRow, err = strconv.Atoi(row); err != nil {
		return Screen{}, fmt.Errorf("cursor row %q: %w", row, err)
	}
	if s.CursorCol, err = strconv.Atoi(col); err != nil {
		return Screen{}, fmt.Errorf("cursor column %q: %w", col, err)
	}
	s.CursorHidden = fields[2] == "0"

	// capture-pane drops rows that are entirely empty at the bottom, and trims each row's trailing
	// spaces. The rows are padded back so that a recording always describes the whole screen; the
	// trimming within a row is what the package doc says is ignored on both sides.
	lines := expandTabs(strings.Split(strings.TrimRight(content, "\n"), "\n"), cols)
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	s.Lines = lines[:rows]

	styledLines := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	styles := styledScreen(styledLines)
	for r := 0; r < rows; r++ {
		if r < len(styles) {
			s.Styles = append(s.Styles, styles[r])
			continue
		}
		s.Styles = append(s.Styles, nil)
	}

	if h := strings.TrimRight(hist, "\n"); h != "" {
		s.History = strings.Split(h, "\n")
	}
	return s, nil
}

// tmuxConfig is the terminal the corpus is recorded against.
//
// Written out rather than relying on defaults so that a recording says what it was made with. Only
// what changes a screen belongs here.
const tmuxConfig = `# Written by internal/vt/conform. Not the user's configuration, deliberately.
set -g history-limit 1000
set -g default-terminal "xterm-256color"
`

// The tmux wait-for channels the pane and the recorder take turns on.
const (
	doneSignal = "conform-written"
	stepSignal = "conform-step"
	goSignal   = "conform-go"
)

// waitForWrites blocks until the pane has finished writing the case.
//
// Stability alone is not enough, and this was measured rather than reasoned: `settle` on its own
// recorded an empty screen for a case whose first capture happened before the shell had started
// `cat`. Two identical *empty* captures is a stable screen, so the harness recorded blankness and
// called it the truth - the precise shape of green lie this package exists to catch, found in the
// package itself on its first run.
//
// tmux wait-for is signalled by the pane's own shell after cat returns, so it cannot fire early.
func waitFor(tmux, sock, channel string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmux, "-S", sock, "wait-for", channel).CombinedOutput()
	if err != nil {
		return fmt.Errorf("waiting on %s: %v: %s", channel, err, out)
	}
	return nil
}

// expandTabs replaces a captured tab with the blanks it stands for.
//
// Assuming a stop every eight columns, which is all it can do: capture-pane emits a literal tab
// where the cursor jumped and says nothing about where it jumped to. A case that moves a tab stop
// therefore cannot be recorded correctly - the recording puts the text at the default stop, which
// is not where the terminal put it - so those cases live in the emulator's own tests instead. Every
// case in the corpus uses the default stops.
//
// capture-pane re-emits a tab where the cursor jumped rather than the blank cells the screen
// actually holds, so a recording of a tab compared as a literal tab character against an emulator
// that (correctly) holds spaces. The screen is what is being compared, and on the screen those
// cells are blank.
func expandTabs(lines []string, cols int) []string {
	for i, l := range lines {
		if !strings.Contains(l, "\t") {
			continue
		}
		var b strings.Builder
		width := 0
		for _, r := range l {
			if r != '\t' {
				b.WriteRune(r)
				width += vt.RuneWidth(r)
				continue
			}
			// Columns, not bytes. Padding by byte length made a three-byte replacement character
			// count as three columns, so a tab after one landed two columns early and the
			// comparison blamed the emulator for the oracle's arithmetic.
			//
			// And never past the last column. A tab stop beyond the edge of the screen is the
			// edge of the screen - a terminal clamps there - but this expanded the full eight and
			// produced a recording with a character in column 40 of a forty-column screen, which
			// no screen can hold. The emulator was reported wrong for putting it in column 39.
			// The last column, not the width: a tab stop past the edge leaves the cursor on the
			// final column, and clamping to the width itself filled every remaining cell and left
			// the character after the tab with nowhere to go - producing a recorded row of
			// forty-one columns on a forty-column screen.
			next := min((width/8+1)*8, max(cols-1, 0))
			for ; width < next; width++ {
				b.WriteByte(' ')
			}
		}
		lines[i] = strings.TrimRight(b.String(), " ")
	}
	return lines
}

// settle waits until the pane has stopped changing.
//
// Honestly: this is a guard whose necessity has not been demonstrated. The race it covers is real
// in principle - the wait-for signal travels over the tmux socket while the bytes travel over the
// pty, so nothing orders them - but the corpus was run 60 times with this removed and never
// disagreed with a recording. It is kept because it costs about sixty milliseconds a case and the
// alternative is a harness that is wrong rarely, which is the worst frequency to be wrong at.
//
// If it ever needs to earn its place properly, the thing to write is a case whose stream ends in a
// large burst, and to show it failing without this.
func settle(tm func(...string) (string, error)) error {
	prev := ""
	stable := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		now, err := tm("capture-pane", "-p")
		if err != nil {
			return err
		}
		if now == prev {
			stable++
			if stable == 2 {
				return nil
			}
		} else {
			stable = 0
		}
		prev = now
		time.Sleep(30 * time.Millisecond)
	}
	return fmt.Errorf("the pane was still changing after 10s")
}
