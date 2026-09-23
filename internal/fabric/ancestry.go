package fabric

import (
	"os"
	"strconv"
	"strings"
)

// DescendsFrom reports whether pid is anc or runs somewhere beneath it.
//
// This is how the client knows it has been started inside the service it is about to attach to.
// The first answer was the GOZELLIJ variable, which names the service - but a name can change and
// a variable in a running shell cannot, and a service is free to set GOZELLIJ to anything. The
// process tree says it without either weakness.
//
// A process that has been reparented (a daemonised child, adopted by init or a subreaper) no
// longer descends from anything it came from, so this can answer false for something that did
// start inside anc. It never answers true wrongly, which is the direction that matters for a
// guard: a false negative is the behaviour from before the check existed.
func DescendsFrom(pid, anc int) bool {
	if anc <= 1 || pid <= 0 {
		return false
	}
	// Bounded, because pid reuse could in principle close a loop between reads, and a guard that
	// hangs is worse than one that misses.
	for i := 0; i < 4096 && pid > 1; i++ {
		if pid == anc {
			return true
		}
		parent, ok := parentOf(pid)
		if !ok {
			return false
		}
		pid = parent
	}
	return false
}

// parentOf is field 4 of /proc/<pid>/stat, read after the last ')' because the command name
// before it may itself contain spaces and parentheses.
func parentOf(pid int) (int, bool) {
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	close := strings.LastIndexByte(string(body), ')')
	if close < 0 || close+2 >= len(body) {
		return 0, false
	}
	fields := strings.Fields(string(body)[close+2:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
