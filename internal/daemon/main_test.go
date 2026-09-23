package daemon

import (
	"os"
	"testing"
)

// TestMain keeps every daemon this package starts away from the real service manager.
//
// Run from inside a gozellij shell that was started before services stopped inheriting it,
// NOTIFY_SOCKET is set - and a throwaway daemon in a test handed its listening socket to the real
// systemd over it, then hung trying to close the socket. The whole package sat out the ten-minute
// test timeout, with the goroutine stuck in net.(*UnixListener).Close.
//
// Once, for the whole binary, rather than in each helper: the first attempt fixed the two helpers
// and the next hang was in a test that calls Listen itself. A test that would like a fake service
// manager sets NOTIFY_SOCKET to one of its own, which overrides this.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"NOTIFY_SOCKET", "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES",
		"INVOCATION_ID", "JOURNAL_STREAM", "WATCHDOG_PID", "WATCHDOG_USEC",
	} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
