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

	// The input goes through a file rather than the command line: a case is arbitrary bytes,
	// including NUL and escape, and quoting those into a shell command is a way to test the
	// quoting instead of the terminal.
	in := filepath.Join(dir, "in")
	if err := os.WriteFile(in, c.Input, 0o600); err != nil {
		return Screen{}, err
	}
	sock := filepath.Join(dir, "sock")

	// A fixed configuration, never the user's. tmux reads ~/.tmux.conf by default, so a recording
	// made here would carry whatever that file says - and on the machine this corpus was first
	// recorded on it says `set -g history-limit 0`, which silently made every recording of
	// scrollback empty. A corpus that depends on whose laptop it was taken on is not an oracle.
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte(tmuxConfig), 0o600); err != nil {
		return Screen{}, err
	}

	tm := func(args ...string) (string, error) {
		out, err := exec.Command(tmux, append([]string{"-f", conf, "-S", sock}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out), nil
	}

	// stty -echo, then cat, then signal, then sleep, in one shell. The pane must stay alive after
	// the bytes are drawn, because a pane that exits takes the screen with it and there would be
	// nothing to capture; the signal in the middle is what says the bytes have all been written.
	//
	// The -echo is not tidiness. A real program's output contains device queries - vim asks for
	// the cursor position and the terminal's identity - and tmux answers them on the pty. With
	// echo on, the pane's own terminal discipline writes those answers back to the screen, and a
	// recording of vim came out with `^[[2;2R^[[3;1R^[[>84;0;0c` drawn across its first row. The
	// oracle was recording its own replies as if they were the program's output.
	if _, err := tm("new-session", "-d", "-x", strconv.Itoa(c.Cols), "-y", strconv.Itoa(c.Rows),
		"sh", "-c", fmt.Sprintf("stty -echo; cat %s; %s -S %s wait-for -S %s; sleep 300", in, tmux, sock, doneSignal)); err != nil {
		return Screen{}, err
	}
	defer tm("kill-server")

	if err := waitForWrites(tmux, sock); err != nil {
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

	pos, err := tm("display", "-p", "#{cursor_y} #{cursor_x}")
	if err != nil {
		return Screen{}, err
	}
	row, col, ok := strings.Cut(strings.TrimSpace(pos), " ")
	if !ok {
		return Screen{}, fmt.Errorf("tmux reported the cursor as %q", pos)
	}
	s := Screen{Cols: c.Cols, Rows: c.Rows}
	if s.CursorRow, err = strconv.Atoi(row); err != nil {
		return Screen{}, fmt.Errorf("cursor row %q: %w", row, err)
	}
	if s.CursorCol, err = strconv.Atoi(col); err != nil {
		return Screen{}, fmt.Errorf("cursor column %q: %w", col, err)
	}

	// capture-pane drops rows that are entirely empty at the bottom, and trims each row's trailing
	// spaces. The rows are padded back so that a recording always describes the whole screen; the
	// trimming within a row is what the package doc says is ignored on both sides.
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	for len(lines) < c.Rows {
		lines = append(lines, "")
	}
	s.Lines = lines[:c.Rows]

	styledLines := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	styles := styledScreen(styledLines)
	for r := 0; r < c.Rows; r++ {
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

// doneSignal is the tmux wait-for channel the pane signals once it has written everything.
const doneSignal = "conform-written"

// waitForWrites blocks until the pane has finished writing the case.
//
// Stability alone is not enough, and this was measured rather than reasoned: `settle` on its own
// recorded an empty screen for a case whose first capture happened before the shell had started
// `cat`. Two identical *empty* captures is a stable screen, so the harness recorded blankness and
// called it the truth - the precise shape of green lie this package exists to catch, found in the
// package itself on its first run.
//
// tmux wait-for is signalled by the pane's own shell after cat returns, so it cannot fire early.
func waitForWrites(tmux, sock string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmux, "-S", sock, "wait-for", doneSignal).CombinedOutput()
	if err != nil {
		return fmt.Errorf("waiting for the case to be written: %v: %s", err, out)
	}
	return nil
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
