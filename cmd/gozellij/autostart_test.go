package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The block gezellij's own setup script writes, as found on a real machine. Kept verbatim because
// the check has to match what people actually have, not what a tidy example would look like.
const gezellijBlock = `# >>> gezellij login setup >>>
GEZELLIJ_BIN="/usr/bin/gezellij"
GEZELLIJ_SESSION="main"
export ZELLIJ_AUTO_ATTACH=true
if [[ $- == *i* ]] \
   && [ -z "${ZELLIJ:-}" ] && [ -z "${TMUX:-}" ] && [ -z "${STY:-}" ] \
   && [ -x "$GEZELLIJ_BIN" ]; then
    _gezellij_auto_start() {
        "$GEZELLIJ_BIN" setup --generate-auto-start bash
    }
    eval "$(_gezellij_auto_start)"
fi
# <<< gezellij login setup <<<
`

func homeWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNoStartupFilesIsNothingToWarnAbout(t *testing.T) {
	if got := findAutostarts(homeWith(t, nil)); len(got) != 0 {
		t.Fatalf("found %d autostarts in an empty home: %+v", len(got), got)
	}
}

func TestAnotherMultiplexersAutostartIsFound(t *testing.T) {
	home := homeWith(t, map[string]string{".profile": gezellijBlock})
	got := findAutostarts(home)
	if len(got) == 0 {
		t.Fatal("the block a real machine has was not found")
	}
	for _, f := range got {
		if f.Program != "gezellij" {
			t.Errorf("named %q, want gezellij: %+v", f.Program, f)
		}
		if f.Guarded {
			t.Errorf("reported as already aware of gozellij: %+v", f)
		}
	}
}

func TestABlockThatAlreadyChecksGozellijIsLeftAlone(t *testing.T) {
	// Somebody who has added the guard has thought about this, and saying it again would be
	// noise on every run of doctor.
	guarded := strings.Replace(gezellijBlock,
		`&& [ -x "$GEZELLIJ_BIN" ]; then`,
		`&& [ -z "${GOZELLIJ:-}" ] && [ -x "$GEZELLIJ_BIN" ]; then`, 1)
	for _, f := range findAutostarts(homeWith(t, map[string]string{".profile": guarded})) {
		if !f.Guarded {
			t.Errorf("a block that checks $GOZELLIJ was still reported as a problem: %+v", f)
		}
	}
}

func TestACommentedOutBlockIsNotLive(t *testing.T) {
	// The gezellij setup script leaves its block behind commented out when it is undone.
	// Reporting that as live is crying wolf, and the next warning gets ignored with it.
	var commented strings.Builder
	for _, line := range strings.Split(gezellijBlock, "\n") {
		commented.WriteString("# " + line + "\n")
	}
	if got := findAutostarts(homeWith(t, map[string]string{".profile": commented.String()})); len(got) != 0 {
		t.Fatalf("a commented-out block was reported as live: %+v", got)
	}
}

func TestTheWordAloneIsNotALaunch(t *testing.T) {
	// A PATH containing the word, a variable named after it, an alias. None of these start
	// anything, and a check that fires on them is a check people learn to ignore.
	for _, body := range []string{
		"export PATH=$PATH:/opt/zellij/bin\n",
		"export ZELLIJ_AUTO_ATTACH=true\n",
		"alias z='zellij'\n",
		"# zellij attach -c main\n",
		"export TMUX_TMPDIR=/tmp\n",
	} {
		if got := findAutostarts(homeWith(t, map[string]string{".bashrc": body})); len(got) != 0 {
			t.Errorf("%q was read as a multiplexer launch: %+v", strings.TrimSpace(body), got)
		}
	}
}

func TestEachMultiplexerIsNamedSoTheFixCanBe(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"tmux attach -t main || tmux new-session -s main\n", "tmux"},
		{"screen -R main\n", "screen"},
		{"_byobu_sourced=1 . /usr/bin/byobu-launch\n", "byobu"},
		{"zellij attach -c main\n", "zellij"},
	} {
		got := findAutostarts(homeWith(t, map[string]string{".bashrc": c.body}))
		if len(got) == 0 {
			t.Errorf("%q was not found at all", strings.TrimSpace(c.body))
			continue
		}
		if got[0].Program != c.want {
			t.Errorf("%q was named %q, want %q", strings.TrimSpace(c.body), got[0].Program, c.want)
		}
		// The fix has to be the thing to type, and for the ones with their own undo command it
		// has to be that command rather than "edit this file by hand".
		if fix := autostartFix(got[0].Program); fix == "" {
			t.Errorf("%s has no fix to offer", got[0].Program)
		}
	}
}

func TestTheInteractiveFilesAreReadToo(t *testing.T) {
	// ~/.profile is what an interactive *login* shell reads, which is what gozellij starts.
	// ~/.bashrc is what everything opened inside a pane afterwards reads. A block in either one
	// fires inside a gozellij session, and only checking the login files would miss half of it.
	for _, name := range []string{".profile", ".bashrc", ".zshrc", ".bash_profile", ".zprofile"} {
		if got := findAutostarts(homeWith(t, map[string]string{name: "tmux attach -t main\n"})); len(got) == 0 {
			t.Errorf("a block in ~/%s was not looked at", name)
		}
	}
}
