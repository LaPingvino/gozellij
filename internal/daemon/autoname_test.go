package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestATabIsNamedAfterItsProjectOrItsTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "My Website")
	deep := filepath.Join(repo, "src", "pages")
	notes := filepath.Join(home, "notes")
	for _, d := range []string{filepath.Join(repo, ".git"), deep, notes} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct{ comm, cwd, want string }{
		{"claude", repo, "my-website"}, // the project, made a valid name
		{"vim\n", deep, "my-website"},  // deep inside it: still the project
		{"claude", notes, "notes"},     // Claude Code outside a repository: its directory
		{"vim", notes, "vim"},          // anything else outside one: the tool
		{"claude", home, "claude"},     // the home directory names nothing
		{"htop", "/", "htop"},
		{"bash", repo, ""}, // a shell in a shell names nothing
		{"sudo", repo, ""},
	}
	for _, c := range cases {
		got := ""
		if tool := toolName(c.comm); tool != "" {
			got = tabName(tool, c.cwd)
		}
		if got != c.want {
			t.Errorf("%q in %s: %q, want %q", c.comm, c.cwd, got, c.want)
		}
	}
}

// A tab already named after its project is not renamed to the same thing, suffix or not.
func TestSameNameIgnoresTheUniquenessSuffix(t *testing.T) {
	for _, c := range []struct {
		name, base string
		want       bool
	}{
		{"gozellij", "gozellij", true},
		{"gozellij-2", "gozellij", true},
		{"gozellij-dev", "gozellij", false},
		{"gozellij-", "gozellij", false},
		{"vim", "gozellij", false},
	} {
		if got := sameName(c.name, c.base); got != c.want {
			t.Errorf("sameName(%q, %q) = %v", c.name, c.base, got)
		}
	}
}
