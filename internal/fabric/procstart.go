package fabric

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// When a process actually started, as opposed to when we noticed it.
//
// This matters for one case and it is the case this whole file exists for: a service adopted after
// a daemon crash. The replacement has no idea when the process began - the daemon that knew died -
// so the first version recorded the adoption as the start, with a comment claiming the error would
// be "seconds". It is not seconds. It is the entire time since the crash, and it grows: a shell
// running since yesterday reported twenty minutes of uptime because that is how long ago the
// daemon came back. Found in production, on the first real crash this program survived.
//
// The kernel knows. /proc/<pid>/stat field 22 is the process's start, in clock ticks since boot,
// and /proc/stat's btime is when boot was. Checked against `ps -o lstart` on two live processes:
// exact to the second.

// userHZ is the unit of field 22.
//
// Hardcoded, and it is the one number here that is not read from the machine. Linux's USER_HZ has
// been 100 on every architecture Go builds for since the constant was introduced, and the only way
// to ask is sysconf(_SC_CLK_TCK) through cgo, which this program does not use for anything and
// will not start using for a uptime. If it is ever wrong the symptom is a uniformly scaled
// uptime, which is visible rather than silent.
const userHZ = 100

// ProcessStart reports when a process began, or false when it cannot be worked out.
//
// False rather than a guess: the caller has a sensible fallback (now), and a start time invented
// here would be indistinguishable from one the kernel gave us.
func ProcessStart(pid int) (time.Time, bool) {
	boot, ok := bootTime()
	if !ok {
		return time.Time{}, false
	}
	ticks, ok := startTicks(pid)
	if !ok {
		return time.Time{}, false
	}
	return boot.Add(time.Duration(ticks) * time.Second / userHZ), true
}

func bootTime() (time.Time, bool) {
	body, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(body), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(secs, 0), true
	}
	return time.Time{}, false
}

// startTicks reads field 22 of /proc/<pid>/stat.
//
// Counted from the last ')' rather than by splitting the whole line, because field 2 is the
// executable name in parentheses and a process may be called "my (weird) name" - which shifts
// every field after it and is exactly the kind of thing nobody notices until somebody does it.
func startTicks(pid int) (int64, bool) {
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	close := strings.LastIndexByte(string(body), ')')
	if close < 0 || close+2 >= len(body) {
		return 0, false
	}
	// After "(comm)" the next field is state, which is field 3. starttime is field 22, so it is
	// the 20th of what follows.
	fields := strings.Fields(string(body)[close+2:])
	const startTimeIndex = 22 - 3
	if len(fields) <= startTimeIndex {
		return 0, false
	}
	ticks, err := strconv.ParseInt(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, false
	}
	return ticks, true
}
