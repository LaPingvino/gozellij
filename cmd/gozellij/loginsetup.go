package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Making gozellij the thing your login shell starts.
//
// This edits a file that decides whether you can log in, on a machine you may only be able to
// reach by logging in. Everything here is built around that one sentence.
//
//   - It prints what it would do and changes nothing, unless you pass -install. A command that
//     rewrites your profile the moment you type its name is not a command, it is a trap.
//   - It copies the file first, to <file>.gozellij-<timestamp>, and says where.
//   - The block it writes never execs. If gozellij cannot start - the daemon is down, the binary
//     was replaced mid-upgrade, the socket path is too long - you get your shell and a line
//     saying why. The usual snippet for this kind of thing execs, and the failure mode of that is
//     an ssh session that closes the instant it opens, on a box whose only way in is ssh.
//   - It refuses to leave two multiplexers starting each other, and it comments the other one out
//     rather than deleting it, with markers -undo puts back.
//
// The guard is the same shape as everybody else's, with the addition that makes it a good citizen:
// it stands down for $GOZELLIJ *and* for $ZELLIJ, $TMUX and $STY. A multiplexer that only knows
// about itself is how this problem got here.

const (
	loginStartMarker = "# >>> gozellij login setup >>>"
	loginEndMarker   = "# <<< gozellij login setup <<<"
	disabledStart    = "# >>> disabled by gozellij login-setup >>>"
	disabledEnd      = "# <<< disabled by gozellij login-setup <<<"
)

// loginBlock is what gets written. %s is the service to land in.
func loginBlock(service string) string {
	return loginStartMarker + `
# Added by ` + "`gozellij login-setup`" + `. Remove with: gozellij login-setup -undo
#
# No exec: if gozellij cannot start you keep this shell and are told why, rather than
# having your session end as it begins.
if [ -n "${PS1:-}" ] && case "$-" in *i*) true;; *) false;; esac \
   && [ -z "${GOZELLIJ:-}" ] && [ -z "${ZELLIJ:-}" ] && [ -z "${TMUX:-}" ] && [ -z "${STY:-}" ] \
   && [ -z "${SSH_ORIGINAL_COMMAND:-}" ] \
   && [ -z "${GOZELLIJ_NO_AUTOSTART:-}" ] \
   && [ ! -e "${XDG_CONFIG_HOME:-$HOME/.config}/gozellij/no-autostart" ] \
   && [ -n "${TERM:-}" ] && [ "$TERM" != "dumb" ] && [ "$TERM" != "linux" ] \
   && command -v gozellij >/dev/null 2>&1; then
    gozellij ` + service + ` || printf '%s\n' "gozellij: could not start; you are in a plain shell (opt out: touch ~/.config/gozellij/no-autostart)"
fi
` + loginEndMarker + "\n"
}

func cmdLoginSetup(args []string) error {
	fs := flag.NewFlagSet("login-setup", flag.ContinueOnError)
	file := fs.String("file", "", "the startup file to edit (default: the one your login shell reads)")
	service := fs.String("name", DefaultShellService, "the service to land in")
	install := fs.Bool("install", false, "actually make the changes (without this, it only says what it would do)")
	undo := fs.Bool("undo", false, "take the block out again and put back whatever it disabled")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot find your home directory: %w", err)
	}
	target := *file
	if target == "" {
		target = filepath.Join(home, loginFileFor(os.Getenv("SHELL"), home))
	}

	if *undo {
		return loginUndo(target, *install)
	}
	return loginInstall(home, target, *service, *install)
}

// loginFileFor picks the file this person's login shell actually reads.
//
// bash is the awkward one: an interactive *login* bash reads ~/.bash_profile if it exists and
// ~/.profile otherwise, and never ~/.bashrc unless one of those sources it. Writing to the wrong
// one is a block that never runs and a person who thinks the program is broken.
func loginFileFor(shell, home string) string {
	switch {
	case strings.Contains(shell, "zsh"):
		return ".zprofile"
	case strings.Contains(shell, "bash"):
		if _, err := os.Stat(filepath.Join(home, ".bash_profile")); err == nil {
			return ".bash_profile"
		}
		return ".profile"
	default:
		return ".profile"
	}
}

func loginInstall(home, target, service string, doIt bool) error {
	// Somebody else's block first: two multiplexers starting each other is worse than neither.
	others := findAutostarts(home)
	var live []autostartFinding
	for _, f := range others {
		if !f.Guarded && f.Program != "gozellij" {
			live = append(live, f)
		}
	}

	existing, _ := os.ReadFile(target)
	already := strings.Contains(string(existing), loginStartMarker)

	fmt.Printf("startup file:  %s\n", short(target))
	if already {
		fmt.Println("gozellij:      already set up here; it will be replaced with the current version")
	} else {
		fmt.Printf("gozellij:      a block will be added that runs `gozellij %s` on an interactive login\n", service)
	}
	// One line per file, not per match: a block that launches a multiplexer mentions it on
	// several lines, and listing each one reads like ten problems instead of one.
	files := map[string]bool{}
	var order []string
	firstIn := map[string]autostartFinding{}
	for _, f := range live {
		if !files[f.File] {
			files[f.File] = true
			order = append(order, f.File)
			firstIn[f.File] = f
		}
	}
	for _, path := range order {
		f := firstIn[path]
		fmt.Printf("disable:       %s:%d starts %s - it will be commented out, with markers -undo puts back\n",
			short(path), f.Line, f.Program)
	}
	if len(live) == 0 {
		fmt.Println("disable:       nothing else in your startup files starts a multiplexer")
	}
	if !doIt {
		fmt.Println("\nNothing has been changed. To do it:  gozellij login-setup -install")
		return nil
	}

	for path := range files {
		if err := disableOthersIn(path); err != nil {
			return err
		}
	}
	if err := writeLoginBlock(target, service); err != nil {
		return err
	}
	fmt.Printf("\ndone. Check it with:  gozellij doctor\n")
	fmt.Printf("Open a new login shell to try it; if anything is wrong:  gozellij login-setup -undo -install\n")
	return nil
}

// backedUp is the files already copied during this run.
//
// Once per file, not once per change. This command edits a file twice - commenting out the other
// multiplexer, then adding its own block - and the two copies landed under the same name, because
// the name has a timestamp and the timestamp has seconds in it. The second copy was of the
// half-modified file, so the thing offered as "the original" was not one. Found by the test that
// checks the copy against what the file said before, which is the only way this shows up.
var backedUp = map[string]string{}

// reported is which copies have already been mentioned, so one file is not announced twice.
var reported = map[string]bool{}

// backup copies a file before it is changed, and returns where it went.
func backup(path string) (string, error) {
	if to, done := backedUp[path]; done {
		return to, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	to := fmt.Sprintf("%s.gozellij-%s", path, time.Now().Format("20060102-150405"))
	// And if that name is taken - two runs in the same second - find one that is not, rather
	// than writing over a copy somebody may need.
	for i := 1; ; i++ {
		if _, err := os.Stat(to); os.IsNotExist(err) {
			break
		}
		to = fmt.Sprintf("%s.gozellij-%s-%d", path, time.Now().Format("20060102-150405"), i)
	}
	if err := os.WriteFile(to, body, 0o600); err != nil {
		return "", fmt.Errorf("could not copy %s before changing it: %w", short(path), err)
	}
	backedUp[path] = to
	return to, nil
}

func writeLoginBlock(target, service string) error {
	to, err := backup(target)
	if err != nil {
		return err
	}
	if to != "" && !reported[to] {
		reported[to] = true
		fmt.Printf("copied:        %s\n", short(to))
	}
	body, _ := os.ReadFile(target)
	out := removeBlock(string(body), loginStartMarker, loginEndMarker)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += loginBlock(service)
	return os.WriteFile(target, []byte(out), 0o600)
}

// disableOthersIn comments out every multiplexer launch in one file, wrapped in markers.
//
// Commented rather than deleted, and one block per file rather than per line, because these
// launches live inside an `if` and taking out the middle of one leaves a shell script that does
// not parse - which on a login file is the lockout this whole command is arranged to avoid.
func disableOthersIn(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	to, err := backup(path)
	if err != nil {
		return err
	}
	if to != "" && !reported[to] {
		reported[to] = true
		fmt.Printf("copied:        %s\n", short(to))
	}

	lines := strings.Split(string(body), "\n")
	first, last := -1, -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !startsAMultiplexer(line) {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return nil
	}
	// Widen to the whole `if` the launch sits in, by walking out to the surrounding block.
	first, last = widenToBlock(lines, first, last)

	var out []string
	out = append(out, lines[:first]...)
	out = append(out, disabledStart)
	for _, line := range lines[first : last+1] {
		out = append(out, "#gz# "+line)
	}
	out = append(out, disabledEnd)
	out = append(out, lines[last+1:]...)
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600)
}

// widenToBlock grows a range to cover the shell `if` around it, so that commenting it out leaves
// something that still parses.
func widenToBlock(lines []string, first, last int) (int, int) {
	for i := first; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "if ") || strings.HasPrefix(t, "if[") {
			first = i
			break
		}
		// A continuation line ends with a backslash; keep walking past those.
		if i < first && !strings.HasSuffix(t, "\\") && t != "" && !strings.HasPrefix(t, "#") {
			break
		}
	}
	depth := 0
	for i := first; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "if ") {
			depth++
		}
		if t == "fi" || strings.HasPrefix(t, "fi ") || strings.HasPrefix(t, "fi;") {
			depth--
			if depth <= 0 {
				return first, max(i, last)
			}
		}
	}
	return first, last
}

func startsAMultiplexer(line string) bool {
	for _, m := range multiplexerStarts {
		for _, needle := range m.needles {
			if strings.Contains(line, needle) {
				return true
			}
		}
	}
	return false
}

func loginUndo(target string, doIt bool) error {
	home, _ := os.UserHomeDir()
	fmt.Printf("startup file:  %s\n", short(target))
	fmt.Println("gozellij:      its block will be taken out")
	fmt.Println("restore:       anything it commented out will be put back")
	if !doIt {
		fmt.Println("\nNothing has been changed. To do it:  gozellij login-setup -undo -install")
		return nil
	}
	if to, err := backup(target); err == nil && to != "" {
		fmt.Printf("copied:        %s\n", short(to))
	}
	body, err := os.ReadFile(target)
	if err == nil {
		if err := os.WriteFile(target, []byte(removeBlock(string(body), loginStartMarker, loginEndMarker)), 0o600); err != nil {
			return err
		}
	}
	// Whatever was disabled, wherever it was.
	for _, name := range autostartFiles {
		path := filepath.Join(home, name)
		b, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(b), disabledStart) {
			continue
		}
		if to, berr := backup(path); berr == nil && to != "" {
			fmt.Printf("copied:        %s\n", short(to))
		}
		if err := os.WriteFile(path, []byte(reenable(string(b))), 0o600); err != nil {
			return err
		}
		fmt.Printf("restored:      %s\n", short(path))
	}
	fmt.Println("\ndone.")
	return nil
}

// removeBlock takes out everything between two markers, inclusive.
func removeBlock(body, start, end string) string {
	var out []string
	inside := false
	scan := bufio.NewScanner(strings.NewReader(body))
	for scan.Scan() {
		line := scan.Text()
		switch {
		case strings.TrimSpace(line) == start:
			inside = true
		case strings.TrimSpace(line) == end:
			inside = false
		case !inside:
			out = append(out, line)
		}
	}
	joined := strings.Join(out, "\n")
	if body != "" && !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	return joined
}

// reenable undoes what disableOthersIn did.
func reenable(body string) string {
	var out []string
	scan := bufio.NewScanner(strings.NewReader(body))
	for scan.Scan() {
		line := scan.Text()
		t := strings.TrimSpace(line)
		if t == disabledStart || t == disabledEnd {
			continue
		}
		out = append(out, strings.TrimPrefix(line, "#gz# "))
	}
	joined := strings.Join(out, "\n")
	if !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	return joined
}
