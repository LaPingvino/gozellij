package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// This command edits the file that decides whether you can log in, on a machine you may only be
// able to reach by logging in. These are the properties that follow from that sentence, and the
// important ones are checked by running a real bash rather than by reading the generated text.

func TestTheFileAnInteractiveLoginShellReadsIsTheOneChosen(t *testing.T) {
	home := t.TempDir()
	// bash: ~/.bash_profile wins if it exists, ~/.profile otherwise, and never ~/.bashrc -
	// an interactive login bash does not read that one, so a block there would never fire.
	if got := loginFileFor("/bin/bash", home); got != ".profile" {
		t.Errorf("bash with no .bash_profile: %q, want .profile", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loginFileFor("/bin/bash", home); got != ".bash_profile" {
		t.Errorf("bash with a .bash_profile: %q, want .bash_profile", got)
	}
	if got := loginFileFor("/usr/bin/zsh", home); got != ".zprofile" {
		t.Errorf("zsh: %q, want .zprofile", got)
	}
}

func TestWhatItWritesIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash to check the syntax with")
	}
	f := filepath.Join(t.TempDir(), "profile")
	if err := os.WriteFile(f, []byte(loginBlock("shell")), 0o600); err != nil {
		t.Fatal(err)
	}
	// A login file that does not parse is the lockout this command exists to avoid.
	if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("the block does not parse: %v\n%s", err, out)
	}
}

// runLoginShell starts an interactive login bash on a real terminal with the given home, feeds it
// a line, and returns everything it printed.
//
// A real terminal because the guard starts with "is this interactive", and a shell whose input is
// a pipe is not. Getting that wrong once already produced a measurement that said a login shell
// was safe when it was not.
func runLoginShell(t *testing.T, home string, extraPath string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command("bash", "-l")
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + extraPath + ":/usr/bin:/bin",
		"TERM=xterm-256color",
		"SHELL=/bin/bash",
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	go func() {
		time.Sleep(400 * time.Millisecond)
		_, _ = f.WriteString("echo THE-SHELL-IS-MINE\nexit\n")
	}()
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 64*1024)
		var all strings.Builder
		for {
			n, err := f.Read(buf)
			all.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- all.String()
	}()
	select {
	case out := <-done:
		_ = cmd.Wait()
		return out
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the login shell never finished")
		return ""
	}
}

// fakeBin writes a stand-in program that prints and exits with the given code.
func fakeBin(t *testing.T, dir, name, says string, code int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho \"" + says + "\"\nexit " + itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// setUpHome makes a home with the other multiplexer's block in it, runs the install, and returns
// the home and the bin directory.
func setUpHome(t *testing.T) (home, bin string) {
	t.Helper()
	home = t.TempDir()
	bin = filepath.Join(home, "bin")
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(gezellijBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	if err := loginInstall(home, filepath.Join(home, ".profile"), "shell", true); err != nil {
		t.Fatal(err)
	}
	return home, bin
}

func TestAfterSetupTheLoginShellStartsGozellijAndNotTheOther(t *testing.T) {
	home, bin := setUpHome(t)
	fakeBin(t, bin, "gozellij", "GOZELLIJ-STARTED", 0)
	fakeBin(t, bin, "gezellij", "GEZELLIJ-STARTED", 0)

	out := runLoginShell(t, home, bin)
	if !strings.Contains(out, "GOZELLIJ-STARTED") {
		t.Errorf("gozellij was not started by the login shell:\n%s", out)
	}
	if strings.Contains(out, "GEZELLIJ-STARTED") {
		t.Errorf("the other multiplexer started as well, so you would be in two at once:\n%s", out)
	}
}

func TestAGozellijThatCannotStartLeavesYouWithAShell(t *testing.T) {
	// The property this whole command is arranged around. The usual snippet for this kind of
	// thing execs, and then a daemon that is down means an ssh session that closes the instant
	// it opens - on a box whose only way in is ssh.
	home, bin := setUpHome(t)
	fakeBin(t, bin, "gozellij", "GOZELLIJ-FAILED", 3)

	out := runLoginShell(t, home, bin)
	if !strings.Contains(out, "GOZELLIJ-FAILED") {
		t.Fatalf("the block did not even try:\n%s", out)
	}
	if !strings.Contains(out, "THE-SHELL-IS-MINE") {
		t.Fatalf("a gozellij that failed took the shell with it - this is the lockout:\n%s", out)
	}
	if !strings.Contains(out, "could not start") {
		t.Errorf("it failed silently, so nobody would know why they are in a plain shell:\n%s", out)
	}
}

func TestSetupIsUndoneExactly(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	before := gezellijBlock + "\nexport EDITOR=vim\n"
	if err := os.WriteFile(profile, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")

	if err := loginInstall(home, profile, "shell", true); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(profile)
	if !strings.Contains(string(body), loginStartMarker) {
		t.Fatal("the block was not added")
	}
	if !strings.Contains(string(body), disabledStart) {
		t.Fatal("the other multiplexer's block was left running")
	}

	if err := loginUndo(profile, true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(profile)
	if string(after) != before {
		t.Fatalf("undo did not put the file back.\nwant:\n%s\ngot:\n%s", before, after)
	}
}

func TestSettingItUpTwiceLeavesOneBlock(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte("export EDITOR=vim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	for range 3 {
		if err := loginInstall(home, profile, "shell", true); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := os.ReadFile(profile)
	if n := strings.Count(string(body), loginStartMarker); n != 1 {
		t.Fatalf("after three runs there are %d blocks, want 1", n)
	}
}

func TestNothingIsChangedWithoutBeingAskedTwice(t *testing.T) {
	// A command that rewrites your profile the moment you type its name is a trap, so the
	// default says what it would do and stops.
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	before := gezellijBlock
	if err := os.WriteFile(profile, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := loginInstall(home, profile, "shell", false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(profile)
	if string(after) != before {
		t.Fatal("the dry run changed the file")
	}
}

func TestTheOriginalIsCopiedBeforeItIsTouched(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	before := gezellijBlock
	if err := os.WriteFile(profile, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	if err := loginInstall(home, profile, "shell", true); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(home)
	found := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".profile.gozellij-") {
			found = e.Name()
		}
	}
	if found == "" {
		t.Fatal("no copy of the original was kept")
	}
	body, _ := os.ReadFile(filepath.Join(home, found))
	if string(body) != before {
		t.Fatal("the copy is not what the file said before")
	}
}

func TestTheBlockWorksForAServiceThatIsNotCalledShell(t *testing.T) {
	// `gozellij <name>` is not a way to attach to anything: it is an unknown command. It looked
	// like one because the default service is called "shell" and `gozellij shell` is a subcommand
	// that lands there, so the generated block worked for exactly one value of -name and printed
	// the usage text on every login for any other.
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	if err := loginInstall(home, profile, "work", true); err != nil {
		t.Fatal(err)
	}
	// A gozellij that reports how it was called, so the test reads the real invocation rather
	// than the text of the block.
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"CALLED-AS: $*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gozellij"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := runLoginShell(t, home, bin)
	if !strings.Contains(out, "CALLED-AS: shell -name work") {
		t.Fatalf("the block called gozellij in a way that does not attach to `work`:\n%s", out)
	}
}
