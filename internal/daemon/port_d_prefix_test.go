package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from acceptance.sh "the prefix you configured is the prefix you get".

func TestPortDTheConfiguredPrefixIsThePrefixYouGet(t *testing.T) {
	home := screenEnv(t)
	cfg := filepath.Join(home, "prefixed-status")
	if err := os.WriteFile(cfg, []byte("prefix=C-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOZELLIJ_STATUS_CONFIG", cfg)

	_, _, sock := newTestDaemon(t)
	// cat, not a shell: a shell's line editor has its own ideas about Ctrl-], and cat has none.
	dAdd(t, sock, "prefixed", "no", `printf "PREFIXED-UP\r\n"; exec cat`)

	// Wide enough for the whole help on the status row.
	s := attachScreen(t, sock, "prefixed", 300, 8, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("PREFIXED-UP")

	// The help answers the configured key, and names it rather than the default.
	dPrefix(s, 0x02, '?')
	s.BottomShows("detach")
	if b := s.Bottom(); !strings.Contains(b, "Ctrl-B") || strings.Contains(b, "Ctrl-]") {
		t.Errorf("Ctrl-B ? said: %q", b)
	}

	// Ctrl-] is no longer special, so it goes to the service: had the client eaten it, it would
	// have taken the M as a command and cat would echo the marker without it.
	s.Type("\x1dMARKER-PASSED-THROUGH\r")
	s.Shows("MARKER-PASSED-THROUGH")
	if !s.StillAttached() {
		t.Fatal("the attach ended early, so the next check would prove nothing")
	}

	// And Ctrl-B d detaches.
	dPrefix(s, 0x02, 'd')
	if err := s.Ended(); err != nil {
		t.Fatalf("Ctrl-B d: %v", err)
	}
}
