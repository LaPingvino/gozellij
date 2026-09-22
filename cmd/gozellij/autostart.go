package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Another multiplexer's autostart, found before it finds you.
//
// A gozellij shell is `$SHELL -l`, an interactive login shell, which runs your profile. If your
// profile starts a multiplexer - which is exactly what a login multiplexer's setup script puts
// there - then it starts inside gozellij, and you are in two at once with the inner one owning the
// screen.
//
// This is not hypothetical and it is not rare: it is what every one of these setup scripts does,
// and their guards check for their own program. gezellij's checks $ZELLIJ, $TMUX and $STY; tmux's
// usual snippet checks $TMUX. None of them has heard of $GOZELLIJ, so none of them stands down for
// it. The first person to run `gozellij` on a machine set up this way lands in the other
// multiplexer and has no idea why.
//
// So this is a check rather than a fix. Editing somebody's profile behind their back to win a
// fight between two programs is not something this should do on its own.

// autostartFiles are the files a login shell reads, in the order a person would look at them.
//
// Both the login files and the interactive ones: an interactive login bash reads ~/.profile, and
// anything you run from inside a pane afterwards reads ~/.bashrc. A block in either one is a block
// that fires inside a gozellij session.
var autostartFiles = []string{
	".profile", ".bash_profile", ".bash_login", ".bashrc",
	".zprofile", ".zlogin", ".zshrc",
}

// autostartFinding is one multiplexer launch found in a startup file.
type autostartFinding struct {
	File    string
	Line    int
	Program string
	Text    string
	// Guarded is whether the block it is in mentions GOZELLIJ, in which case whoever wrote it
	// has already thought about this and it is not our business.
	Guarded bool
}

// multiplexerStarts are the things that take over a terminal on login. Matched on the command as
// written, not on the word alone: "zellij" in a comment or a PATH is not a launch.
var multiplexerStarts = []struct {
	program string
	needles []string
}{
	{"gezellij", []string{"gezellij attach", "gezellij setup --generate-auto-start", "$GEZELLIJ_BIN"}},
	{"zellij", []string{"zellij attach", "zellij setup --generate-auto-start"}},
	{"tmux", []string{"tmux attach", "tmux new-session", "tmux new -A", "tmux -u new"}},
	{"screen", []string{"screen -R", "screen -x", "screen -dR"}},
	{"byobu", []string{"byobu-launch", "_byobu_sourced"}},
}

// findAutostarts reads the startup files under home and reports what would start a multiplexer.
func findAutostarts(home string) []autostartFinding {
	var out []autostartFinding
	for _, name := range autostartFiles {
		path := filepath.Join(home, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		body, findings := scanAutostart(f, path)
		f.Close()
		// The guard is a property of the file rather than of the line: these blocks span a
		// dozen lines and the condition is nowhere near the launch.
		guarded := strings.Contains(body, "GOZELLIJ")
		for _, fnd := range findings {
			fnd.Guarded = guarded
			out = append(out, fnd)
		}
	}
	return out
}

func scanAutostart(r io.Reader, path string) (string, []autostartFinding) {
	var body strings.Builder
	var out []autostartFinding
	scan := bufio.NewScanner(r)
	line := 0
	for scan.Scan() {
		line++
		text := scan.Text()
		body.WriteString(text)
		body.WriteByte('\n')
		// A commented-out block is somebody who already turned it off. The gezellij setup script
		// leaves one behind when it is undone, and reporting that as live would be crying wolf.
		if strings.HasPrefix(strings.TrimSpace(text), "#") {
			continue
		}
		for _, m := range multiplexerStarts {
			for _, needle := range m.needles {
				if strings.Contains(text, needle) {
					out = append(out, autostartFinding{
						File: path, Line: line, Program: m.program,
						Text: strings.TrimSpace(text),
					})
				}
			}
		}
	}
	return body.String(), out
}

// checkOwnLoginSetup reports whether this is what your login shell starts.
//
// The positive half of the check above. After `gozellij login-setup -install` the honest question
// is not "is anything wrong" but "did it take", and silence is a poor way to answer that - it
// reads the same as not having run the command at all.
//
// It also catches the thing that goes wrong later rather than now: the block names a binary by
// absolute path, and if that path was inside a build directory, `make clean` removes it. Every
// login after that lands in a plain shell, weeks after the build that caused it.
func checkOwnLoginSetup() check {
	home, err := os.UserHomeDir()
	if err != nil {
		return check{name: "login shell", level: levelNote, detail: "cannot find your home directory"}
	}
	for _, name := range autostartFiles {
		path := filepath.Join(home, name)
		body, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(body), loginStartMarker) {
			continue
		}
		binary := namedBinaryIn(string(body))
		switch {
		case binary == "":
			return check{name: "login shell", level: levelOK,
				detail: fmt.Sprintf("%s starts gozellij", short(path))}
		case fileIsThere(binary):
			return check{name: "login shell", level: levelOK,
				detail: fmt.Sprintf("%s starts gozellij, using %s", short(path), binary)}
		default:
			return check{
				name:   "login shell",
				level:  levelWarn,
				detail: fmt.Sprintf("%s starts gozellij but names %s, which is not there any more", short(path), binary),
				fix:    "gozellij login-setup -install    (run it from the gozellij you want it to use)",
			}
		}
	}
	return check{
		name:   "login shell",
		level:  levelNote,
		detail: "nothing in your startup files starts gozellij, so you type it yourself",
		fix:    "gozellij login-setup            (says what it would do; add -install to do it)",
	}
}

// namedBinaryIn pulls the path out of the block's GOZELLIJ_BIN line.
func namedBinaryIn(body string) string {
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GOZELLIJ_BIN=\"")
		if !ok {
			continue
		}
		if end := strings.Index(rest, "\""); end >= 0 {
			return rest[:end]
		}
	}
	return ""
}

func fileIsThere(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// checkAutostart is the doctor check.
func checkAutostart() check {
	home, err := os.UserHomeDir()
	if err != nil {
		return check{name: "login autostart", level: levelNote, detail: "cannot find your home directory: " + err.Error()}
	}
	found := findAutostarts(home)
	var live []autostartFinding
	for _, f := range found {
		if !f.Guarded {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		if len(found) > 0 {
			return check{
				name:   "login autostart",
				level:  levelOK,
				detail: fmt.Sprintf("%s starts a multiplexer but its guard knows about gozellij", short(found[0].File)),
			}
		}
		return check{name: "login autostart", level: levelOK, detail: "nothing in your profile starts another multiplexer"}
	}

	f := live[0]
	more := ""
	if len(live) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(live)-1)
	}
	return check{
		name:  "login autostart",
		level: levelWarn,
		detail: fmt.Sprintf("%s:%d starts %s and does not check $GOZELLIJ%s, so a gozellij shell will "+
			"start %s inside itself - a gozellij shell is a login shell and runs your profile",
			short(f.File), f.Line, f.Program, more, f.Program),
		fix: autostartFix(f.Program),
	}
}

// autostartFix is the thing to type, which depends on who put the block there.
func autostartFix(program string) string {
	switch program {
	case "gezellij":
		return "gezellij-login-setup undo\n" +
			"or, to keep it for now:  add [ -z \"$GOZELLIJ\" ] to the condition in that block"
	case "byobu":
		return "byobu-disable\n" +
			"or, to keep it for now:  add [ -z \"$GOZELLIJ\" ] to the condition in that block"
	default:
		return "remove that block, or add [ -z \"$GOZELLIJ\" ] to its condition"
	}
}

// short writes a path under the home directory as ~/thing, which is how a person refers to it.
func short(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(os.PathSeparator)); ok {
		return "~/" + rest
	}
	return path
}
