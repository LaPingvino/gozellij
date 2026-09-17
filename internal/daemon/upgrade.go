package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/LaPingvino/gozellij/internal/fabric"
)

// HandoverEnv carries the handover manifest across the exec.
//
// An environment variable rather than a file: it cannot be left behind to confuse a later start,
// it cannot be read by another user, and it arrives atomically with the exec that needs it. The
// descriptors it names are the real payload; this is just the map.
const HandoverEnv = "GOZELLIJ_HANDOVER"

// Manifest is what one daemon tells its successor.
type Manifest struct {
	// Version guards against a successor from a different era reading this wrongly. If it does
	// not recognise the number it must refuse the handover and start clean, which loses the
	// processes - but losing them loudly beats adopting them with the wrong field meanings.
	Version int `json:"version"`
	// FromPid is the pid before the exec, which is also the pid after it. Recorded so the
	// successor can say in its log that it really is the same process.
	FromPid   int               `json:"from_pid"`
	Processes []fabric.Handover `json:"processes"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// ManifestVersion is the current handover format.
const ManifestVersion = 1

// PrepareHandover clears FD_CLOEXEC on every live pty so the descriptors survive an exec, and
// returns the manifest describing them.
//
// FD_CLOEXEC is the whole trick. Go sets it on everything it opens, for good reasons - a stray
// descriptor leaking into a child is a classic bug - so the descriptors we *want* to keep have to
// be opted back in, one at a time, deliberately.
func PrepareHandover(fab *fabric.Fabric) (Manifest, []error) {
	var errs []error
	m := Manifest{Version: ManifestVersion, FromPid: os.Getpid()}

	for _, h := range fab.Handovers() {
		if err := clearCloexec(h.PTYFd); err != nil {
			// This process will not survive the upgrade. Say which one and why, rather than
			// discovering it missing afterwards.
			errs = append(errs, fmt.Errorf("service %s: cannot keep pty fd %d across exec: %w",
				h.Name, h.PTYFd, err))
			continue
		}
		m.Processes = append(m.Processes, h)
	}
	return m, errs
}

// clearCloexec marks a descriptor to survive exec.
func clearCloexec(fd int) error {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		return fmt.Errorf("F_GETFD: %w", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD,
		flags&^syscall.FD_CLOEXEC); errno != 0 {
		return fmt.Errorf("F_SETFD: %w", errno)
	}
	return nil
}

// ExecSelf replaces this process with a new copy of the binary, keeping the pid and the
// descriptors named in the manifest.
//
// It does not return on success, because there is no "after" - the image is gone.
func ExecSelf(m Manifest, binary string) error {
	if binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("finding my own binary: %w", err)
		}
		binary = exe
	}
	// Resolve through the symlink so an upgraded binary at the same path is picked up. On Linux
	// os.Executable() reads /proc/self/exe, which after a package upgrade points at the old,
	// deleted inode - exec'ing that would faithfully reinstall the version we are replacing.
	if resolved, err := os.Readlink("/proc/self/exe"); err == nil && resolved != "" {
		// A replaced binary shows as "/path (deleted)"; in that case prefer the original
		// path, which now holds the new file.
		if _, statErr := os.Stat(resolved); statErr == nil {
			binary = resolved
		}
	}

	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding the handover manifest: %w", err)
	}

	env := append(os.Environ(), HandoverEnv+"="+string(payload))
	// Drop any stale manifest inherited from an earlier upgrade, so a successor cannot read a
	// handover that already happened.
	env = withoutDuplicateHandover(env)

	if err := syscall.Exec(binary, os.Args, env); err != nil {
		return fmt.Errorf("exec %s: %w", binary, err)
	}
	return nil // unreachable
}

// withoutDuplicateHandover keeps only the last HandoverEnv entry.
func withoutDuplicateHandover(env []string) []string {
	last := -1
	for i, e := range env {
		if len(e) > len(HandoverEnv) && e[:len(HandoverEnv)+1] == HandoverEnv+"=" {
			last = i
		}
	}
	if last < 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for i, e := range env {
		if len(e) > len(HandoverEnv) && e[:len(HandoverEnv)+1] == HandoverEnv+"=" && i != last {
			continue
		}
		out = append(out, e)
	}
	return out
}

// TakeHandover reads the manifest a predecessor left in the environment, and removes it so a
// later restart of this same process cannot adopt the same descriptors twice.
//
// Returns nil when there is no handover, which is the ordinary case of starting fresh.
func TakeHandover() (*Manifest, error) {
	raw, ok := os.LookupEnv(HandoverEnv)
	if !ok || raw == "" {
		return nil, nil
	}
	_ = os.Unsetenv(HandoverEnv)

	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("the handover manifest from the previous daemon is unreadable: %w", err)
	}
	if m.Version != ManifestVersion {
		// Refuse rather than guess. Adopting descriptors with the wrong field meanings would
		// attach services to the wrong ptys, which is far worse than starting them again.
		return nil, fmt.Errorf("handover manifest is version %d, this daemon speaks version %d; "+
			"refusing to adopt (services will be started fresh)", m.Version, ManifestVersion)
	}
	return &m, nil
}
