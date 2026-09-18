package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

func TestSocketPathTooLongIsAFailureThatSaysWhy(t *testing.T) {
	long := "/tmp/" + strings.Repeat("a", 120) + "/fabric.sock"
	c := checkSocketPath(long)
	if c.level != levelFail {
		t.Errorf("level = %v, want FAIL for a path bind() would reject", c.level)
	}
	if !strings.Contains(c.detail, "too long") {
		t.Errorf("detail = %q; bind's own error never mentions length, so ours has to", c.detail)
	}
	if c.fix == "" {
		t.Error("no fix offered for a problem with an obvious fix")
	}
}

func TestSocketPathOfANormalLengthIsFine(t *testing.T) {
	if c := checkSocketPath("/run/user/1000/gozellij/fabric.sock"); c.level != levelOK {
		t.Errorf("level = %v (%s), want ok", c.level, c.detail)
	}
}

func TestStateDirectoryReadableByOthersIsAWarning(t *testing.T) {
	// A service definition carries its environment, and an environment carries secrets.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Setenv("GOZELLIJ_STATE_DIR", dir)

	c := checkStateDir()
	if c.level != levelWarn {
		t.Errorf("level = %v (%s), want warn for a world-readable state directory", c.level, c.detail)
	}
	if !strings.Contains(c.fix, "chmod 700") {
		t.Errorf("fix = %q, want a chmod", c.fix)
	}
}

func TestStateDirectoryThatIsPrivateIsFine(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Setenv("GOZELLIJ_STATE_DIR", dir)

	if c := checkStateDir(); c.level != levelOK {
		t.Errorf("level = %v (%s), want ok", c.level, c.detail)
	}
}

func TestMissingStateDirectoryIsANoteNotAFault(t *testing.T) {
	// It is created on first use, so its absence is news rather than a problem - but it is news,
	// because somebody looking for their service files needs to know they are not there yet.
	t.Setenv("GOZELLIJ_STATE_DIR", filepath.Join(t.TempDir(), "not-created-yet"))

	if c := checkStateDir(); c.level != levelNote {
		t.Errorf("level = %v (%s), want note", c.level, c.detail)
	}
}

func TestStateDirectoryThatIsAFileIsAFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("GOZELLIJ_STATE_DIR", path)

	if c := checkStateDir(); c.level != levelFail {
		t.Errorf("level = %v (%s), want FAIL", c.level, c.detail)
	}
}

func TestWorstLevelDecidesTheExitCode(t *testing.T) {
	// The contract the exit code encodes: broken now is an error, at risk is a sentence to read.
	// A script that runs doctor in a loop must not fall over because linger is off.
	cases := []struct {
		name  string
		given []check
		want  checkLevel
	}{
		{"all fine", []check{{level: levelOK}, {level: levelOK}}, levelOK},
		{"a note does not escalate", []check{{level: levelOK}, {level: levelNote}}, levelNote},
		{"a warning does not become a failure", []check{{level: levelNote}, {level: levelWarn}}, levelWarn},
		{"a failure wins", []check{{level: levelWarn}, {level: levelFail}, {level: levelOK}}, levelFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := printChecks(tc.given); got != tc.want {
				t.Errorf("worst = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLevelsAreOrderedWorstLast(t *testing.T) {
	// printChecks compares levels with >, so the ordering of the constants is load-bearing
	// rather than cosmetic.
	if !(levelOK < levelNote && levelNote < levelWarn && levelWarn < levelFail) {
		t.Fatal("check levels are not ordered from least to most serious")
	}
}

func TestByteWordIsReadableAtAGlance(t *testing.T) {
	cases := map[int64]string{
		0:                "-",
		-1:               "-",
		512:              "512B",
		2048:             "2.0K",
		1024 * 1024:      "1.0M",
		17 * 1024 * 1024: "17.0M",
		3 << 30:          "3.0G",
		// Just under a boundary: promoting on the raw bytes printed "1024K", a unit that does
		// not exist.
		1024*1024 - 1: "1.0M",
		1<<30 - 1:     "1.0G",
	}
	for n, want := range cases {
		if got := byteWord(n); got != want {
			t.Errorf("byteWord(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestViewerWordSaysNobodyRatherThanZero(t *testing.T) {
	// A column of zeroes reads as a measurement; what is being said is that nobody is there.
	if got := viewerWord(ipc.StatusReply{Viewers: 0}); got != "-" {
		t.Errorf("viewerWord(0) = %q, want -", got)
	}
	if got := viewerWord(ipc.StatusReply{Viewers: 3}); got != "3" {
		t.Errorf("viewerWord(3) = %q, want 3", got)
	}
}
