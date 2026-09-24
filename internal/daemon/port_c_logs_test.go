package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/fabric"
)

// Ported from acceptance.sh "logs are rotated, and only one is kept".

// cLoggedDaemon is newLoggedTestDaemon with the rotation size chosen by the test.
func cLoggedDaemon(t *testing.T, logBytes int64) (sock, logDir string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "gzd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock = filepath.Join(sockDir, SocketName)
	reg, err := fabric.NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatal(err)
	}
	logDir = t.TempDir()
	fab := fabric.NewFabric(reg, fabric.StartOptions{LogDir: logDir, LogBytes: logBytes})
	t.Cleanup(fab.Shutdown)
	srv, err := Listen(sock, fab, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	go func(srv *Server) { _ = srv.Serve() }(srv)
	t.Cleanup(func() { srv.Close() })
	return sock, logDir
}

func cSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// The mechanics at a size a test can afford: rotated at the limit, one generation, still readable.
// The 16 MiB itself is fabric.DefaultLogBytes, which gozellijd gets by leaving LogBytes unset;
// pushing sixteen mebibytes through a pty costs seconds and says nothing more about rotation.
func TestPortCLogsAreRotatedAndOnlyOneGenerationKept(t *testing.T) {
	const limit = 64 << 10
	sock, logDir := cLoggedDaemon(t, limit)
	line := strings.Repeat("x", 99)
	// Well over three limits' worth, so there are at least two rotations: with only one, "only one
	// generation is kept" would be true of a rotation that keeps everything.
	const lines = 4 * limit / 100
	addRunning(t, sock, "noisy", "sh", "-c", fmt.Sprintf(`yes %s | head -n %d; printf 'NOISY-DONE\n'; exec sleep 300`, line, lines))

	live := fabric.LogPath(logDir, "noisy")
	eventually(t, "the service to finish writing", func() bool {
		body, _ := os.ReadFile(live)
		return strings.Contains(string(body), "NOISY-DONE")
	})

	rotated := cSize(live + ".1")
	if rotated < 0 {
		t.Fatalf("no rotated log after %d bytes", cSize(live))
	}
	// Rotation happens before the write that would cross the limit, so the rotated file is at
	// most the limit and short of it by less than one write.
	if rotated > limit || rotated < limit/2 {
		t.Errorf("the rotated log is %d bytes, not about the %d-byte limit", rotated, limit)
	}
	if n := cSize(live); n < 0 || n > limit {
		t.Errorf("the live log is %d bytes, want a fresh one under the %d-byte limit", n, limit)
	}
	if _, err := os.Stat(live + ".2"); err == nil {
		t.Error("a second generation exists, so nothing is ever thrown away")
	}
	matches, _ := filepath.Glob(live + "*")
	for _, m := range matches {
		if base := filepath.Base(m); base != "noisy.log" && base != "noisy.log.1" && !strings.HasSuffix(base, fabric.IndexSuffix) {
			t.Errorf("an extra log file %s", base)
		}
	}

	logs, err := dial(t, sock).Logs("noisy", 4096)
	if err != nil {
		t.Fatalf("logs after rotation: %v", err)
	}
	if !strings.Contains(string(logs.Data), "NOISY-DONE") {
		t.Errorf("logs returned nothing useful after the rotation: %q", logs.Data)
	}
}
