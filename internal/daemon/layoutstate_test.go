package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestASavedLayoutComesBack(t *testing.T) {
	t.Setenv("GOZELLIJ_STATE_DIR", t.TempDir())
	want := savedLayout{How: "rows", Focus: 1, Panes: []savedPane{
		{Service: "shell", Weight: 1},
		{Service: "logs", Weight: 1.4},
	}}
	if err := saveLayout("shell", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadLayout("shell")
	if err != nil || !ok {
		t.Fatalf("loadLayout: %v ok=%v", err, ok)
	}
	if got.signature() != want.signature() {
		t.Fatalf("came back as %q, saved %q", got.signature(), want.signature())
	}
}

func TestALayoutFileIsReadableByHand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOZELLIJ_STATE_DIR", dir)
	if err := saveLayout("shell", savedLayout{How: "columns", Panes: []savedPane{{Service: "logs", Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "layouts", "shell.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The file is meant to be repaired with an editor, so the orientation is a word and the
	// services are named. A test for the format because the format is the promise.
	for _, want := range []string{`"how": "columns"`, `"service": "logs"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("layout file does not contain %s:\n%s", want, data)
		}
	}
}

func TestNoLayoutIsNotAnError(t *testing.T) {
	t.Setenv("GOZELLIJ_STATE_DIR", t.TempDir())
	_, ok, err := loadLayout("never-attached")
	if err != nil || ok {
		t.Fatalf("attaching for the first time: ok=%v err=%v", ok, err)
	}
}

func TestAnUnreadableLayoutSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOZELLIJ_STATE_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "layouts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "layouts", "shell.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := loadLayout("shell"); err == nil || ok {
		t.Fatalf("a corrupt layout read as ok=%v err=%v; it should be reported, not ignored", ok, err)
	}
}

func TestALayoutCannotEscapeItsDirectory(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../../etc/passwd", `a\b`} {
		if _, err := layoutPath(name); err == nil {
			t.Fatalf("%q was accepted as a layout file name", name)
		}
	}
}

func TestTheSignatureChangesWithTheArrangement(t *testing.T) {
	base := savedLayout{How: "columns", Focus: 0, Panes: []savedPane{{Service: "a", Weight: 1}, {Service: "b", Weight: 1}}}
	others := []savedLayout{
		{How: "rows", Focus: 0, Panes: base.Panes},
		{How: "columns", Focus: 1, Panes: base.Panes},
		{How: "columns", Focus: 0, Panes: []savedPane{{Service: "b", Weight: 1}, {Service: "a", Weight: 1}}},
		{How: "columns", Focus: 0, Panes: []savedPane{{Service: "a", Weight: 1.4}, {Service: "b", Weight: 1}}},
		{How: "columns", Focus: 0, Panes: []savedPane{{Service: "a", Weight: 1}}},
	}
	for i, o := range others {
		if o.signature() == base.signature() {
			t.Fatalf("arrangement %d has the same signature as the base (%q), so a change to it would never be written",
				i, base.signature())
		}
	}
	same := savedLayout{How: "columns", Focus: 0, Panes: []savedPane{{Service: "a", Weight: 1}, {Service: "b", Weight: 1}}}
	if same.signature() != base.signature() {
		// Otherwise every pass round the loop writes the file again.
		t.Fatalf("the same arrangement signed differently: %q vs %q", same.signature(), base.signature())
	}
}
