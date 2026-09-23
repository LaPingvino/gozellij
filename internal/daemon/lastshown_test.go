package daemon

import "testing"

func TestTheLastShownServiceFollowsItsRenameOnly(t *testing.T) {
	t.Setenv("GOZELLIJ_STATE_DIR", t.TempDir())
	if got := LastShown(); got != "" {
		t.Fatalf("nothing recorded, but LastShown = %q", got)
	}
	RememberShown("shell")
	if got := LastShown(); got != "shell" {
		t.Fatalf("LastShown = %q", got)
	}
	followRename("shell", "work")
	if got := LastShown(); got != "work" {
		t.Errorf("after shell was renamed to work, LastShown = %q", got)
	}
	// A rename of something else - seen by a terminal that has since been left - must not
	// take the record away from where somebody went.
	followRename("other", "elsewhere")
	if got := LastShown(); got != "work" {
		t.Errorf("an unrelated rename moved the record to %q", got)
	}
}
