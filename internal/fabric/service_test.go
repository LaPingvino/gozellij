package fabric

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func TestValidServiceNameRejectsPathTricks(t *testing.T) {
	// These are the ones that matter: a name becomes a filename, a socket path and a cgroup
	// directory, so anything with a separator or a traversal in it must never reach the disk.
	bad := []string{
		"", "../etc/passwd", "a/b", "a\\b", ".hidden", "-leading", "UPPER",
		"has space", "has.dot", "nul\x00byte", strings.Repeat("a", 65),
	}
	for _, name := range bad {
		if err := ValidServiceName(name); err == nil {
			t.Errorf("ValidServiceName(%q) accepted it, should have refused", name)
		}
	}
	good := []string{"a", "web", "web-1", "web_1", "0", strings.Repeat("a", 64)}
	for _, name := range good {
		if err := ValidServiceName(name); err != nil {
			t.Errorf("ValidServiceName(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateRequiresCommandAndWellFormedEnv(t *testing.T) {
	if err := (&Service{Name: "web"}).Validate(); err == nil {
		t.Error("a service with no command should not validate")
	}
	s := &Service{Name: "web", Command: "sleep", Env: []string{"NOT_KV"}}
	if err := s.Validate(); err == nil {
		t.Error("an env entry without = should not validate")
	}
	s.Env = []string{"KEY=value", "EMPTY="}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}

func TestAddGetRoundTrip(t *testing.T) {
	r := newTestRegistry(t)
	want := Service{
		Name:    "web",
		Command: "/usr/bin/caddy",
		Args:    []string{"run", "--config", "/etc/caddy/Caddyfile"},
		Dir:     "/srv/web",
		Env:     []string{"XDG_CONFIG_HOME=/srv/web/.config"},
		Restart: RestartAlways,
	}
	if err := r.Add(want); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := r.Get("web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != want.Name || got.Command != want.Command || got.Dir != want.Dir {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
	if got.Restart != RestartAlways {
		t.Errorf("Restart = %v, want always", got.Restart)
	}
	if len(got.Args) != len(want.Args) {
		t.Fatalf("Args = %v, want %v", got.Args, want.Args)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt was not stamped")
	}
}

// The on-disk form has to be readable and repairable by a human with a text editor, so the
// restart policy must be a word and not an integer.
func TestOnDiskFormIsHumanReadable(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Add(Service{Name: "db", Command: "postgres", Restart: RestartOnFailure}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir(), "db.json"))
	if err != nil {
		t.Fatalf("reading service file: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, `"restart": "on-failure"`) {
		t.Errorf("restart policy is not stored as a word; file was:\n%s", text)
	}
	// And it must survive a hand edit.
	edited := strings.Replace(text, `"on-failure"`, `"always"`, 1)
	if err := os.WriteFile(filepath.Join(r.Dir(), "db.json"), []byte(edited), 0o600); err != nil {
		t.Fatalf("writing edited file: %v", err)
	}
	s, err := r.Get("db")
	if err != nil {
		t.Fatalf("Get after hand edit: %v", err)
	}
	if s.Restart != RestartAlways {
		t.Errorf("after hand edit Restart = %v, want always", s.Restart)
	}
}

func TestRestartPolicyJSONRejectsNonsense(t *testing.T) {
	var p RestartPolicy
	if err := json.Unmarshal([]byte(`"whenever"`), &p); err == nil {
		t.Error("an unknown policy should be rejected, not silently defaulted")
	}
	if err := json.Unmarshal([]byte(`2`), &p); err == nil {
		t.Error("a numeric policy should be rejected")
	}
}

func TestAddRefusesDuplicateButPutReplaces(t *testing.T) {
	r := newTestRegistry(t)
	s := Service{Name: "web", Command: "one"}
	if err := r.Add(s); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := r.Add(Service{Name: "web", Command: "two"})
	if !errors.Is(err, ErrServiceExists) {
		t.Errorf("second Add error = %v, want ErrServiceExists", err)
	}
	if got, _ := r.Get("web"); got.Command != "one" {
		t.Errorf("a refused Add changed the stored command to %q", got.Command)
	}
	if err := r.Put(Service{Name: "web", Command: "two"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, _ := r.Get("web"); got.Command != "two" {
		t.Errorf("after Put, command = %q, want two", got.Command)
	}
}

func TestGetAndRemoveMissingServiceAreDistinguishable(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.Get("ghost"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("Get missing = %v, want ErrNoSuchService", err)
	}
	if err := r.Remove("ghost"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("Remove missing = %v, want ErrNoSuchService", err)
	}
}

func TestListIsSortedAndReportsBadFilesInsteadOfHidingThem(t *testing.T) {
	r := newTestRegistry(t)
	for _, n := range []string{"web", "api", "db"} {
		if err := r.Add(Service{Name: n, Command: "sleep"}); err != nil {
			t.Fatalf("Add %s: %v", n, err)
		}
	}
	// A service file someone broke by hand.
	if err := os.WriteFile(filepath.Join(r.Dir(), "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing broken file: %v", err)
	}

	got, errs := r.List()
	names := make([]string, len(got))
	for i, s := range got {
		names[i] = s.Name
	}
	if strings.Join(names, ",") != "api,db,web" {
		t.Errorf("List names = %v, want api,db,web (sorted)", names)
	}
	if len(errs) != 1 {
		t.Fatalf("List errors = %v, want exactly one (the broken file must be reported, not skipped)", errs)
	}
	// The complaint has to name the file, because someone has to go and open it.
	if !strings.Contains(errs[0].Error(), "broken.json") {
		t.Errorf("error does not name the offending file: %v", errs[0])
	}
}

// Temp files from an interrupted write must not show up as services.
func TestListIgnoresDotfilesAndTempFiles(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Add(Service{Name: "web", Command: "sleep"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, junk := range []string{".web.123.tmp", ".hidden.json", "notjson.txt"} {
		if err := os.WriteFile(filepath.Join(r.Dir(), junk), []byte("{}"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", junk, err)
		}
	}
	got, errs := r.List()
	if len(errs) != 0 {
		t.Errorf("List errors = %v, want none", errs)
	}
	if len(got) != 1 || got[0].Name != "web" {
		t.Errorf("List = %+v, want just web", got)
	}
}

func TestWriteIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Add(Service{Name: "web", Command: "sleep"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	entries, err := os.ReadDir(r.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temp file survived the write: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("registry holds %d files, want 1", len(entries))
	}
}

func TestServiceFilePermissionsAreNotWorldReadable(t *testing.T) {
	// Service definitions carry environment entries, which carry secrets.
	r := newTestRegistry(t)
	if err := r.Add(Service{Name: "web", Command: "sleep", Env: []string{"TOKEN=hunter2"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	fi, err := os.Stat(filepath.Join(r.Dir(), "web.json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("service file mode = %04o, want no group/other access", mode)
	}
}
