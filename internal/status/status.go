// Package status builds a byobu-style status line.
//
// The widget names and the configuration format follow byobu deliberately, because the people who
// want this have byobu's names in their fingers already: a line reads `uptime load_average memory
// disk date time`, a `#` in front of a name turns it off, and the defaults are the ones byobu
// enables for tmux. What is not copied is byobu's shelling out to a script per widget every few
// seconds - these read /proc directly, because a status line that forks fifteen processes on a
// machine at load 20 is part of the problem it is describing.
//
// Everything here is Linux-specific and says so by returning nothing rather than guessing: a
// widget that cannot answer prints nothing at all, and a line with a gap in it is a better report
// than a line with an invented number in it.
package status

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Context is what the widgets have to work with.
//
// The host facts they read for themselves; the gozellij facts have to be handed to them, because
// only the daemon knows them and a status line is drawn by a client.
type Context struct {
	// Service is the service being displayed, and Position/Of place it among the others -
	// "2/5", the thing a tab bar would show.
	Service  string
	Position int
	Of       int
	// Running and Services count what the fabric is looking after.
	Running  int
	Services int
	// Viewers is how many terminals are attached to this service.
	Viewers int
	// LogBytes is the total size of every service's log.
	LogBytes int64
	// StateDir is what `disk` reports on: the filesystem gozellij is actually spending.
	StateDir string
	// Now is the clock, injectable so tests do not depend on what time it is.
	Now time.Time
}

// A Widget renders one field, or "" when it has nothing to say.
type Widget func(Context) string

// widgets is the registry. Names match byobu's.
var widgets = map[string]Widget{
	"session":      session,
	"services":     services,
	"viewers":      viewers,
	"logs":         logs,
	"uptime":       uptime,
	"load_average": loadAverage,
	"cpu_count":    cpuCount,
	"cpu_freq":     cpuFreq,
	"memory":       memory,
	"disk":         disk,
	"date":         date,
	"time":         clock,
	"release":      release,
	"whoami":       whoami,
	"hostname":     hostname,
}

// Names lists every widget, sorted, for `gozellij stats -list` and for error messages that would
// otherwise leave someone guessing what they may write.
func Names() []string {
	out := make([]string, 0, len(widgets))
	for name := range widgets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Known reports whether a name is a widget.
func Known(name string) bool {
	_, ok := widgets[name]
	return ok
}

// Render lays out one line: left-hand fields, then right-hand fields pushed to the right margin.
//
// When the two halves do not fit, the left is kept and the right is trimmed from its left end -
// the right-hand fields are ordered least to most important in byobu's defaults, so the clock
// survives and the uptime goes first.
func Render(ctx Context, left, right []string, width int) string {
	l := join(ctx, left)
	r := join(ctx, right)

	if width <= 0 {
		if l == "" {
			return r
		}
		if r == "" {
			return l
		}
		return l + "  " + r
	}

	for len(l)+len(r)+1 > width && r != "" {
		if i := strings.Index(r, " "); i >= 0 {
			r = r[i+1:]
			continue
		}
		r = ""
	}
	if len(l) > width {
		l = l[:width]
	}
	gap := width - len(l) - len(r)
	if gap < 0 {
		gap = 0
	}
	return l + strings.Repeat(" ", gap) + r
}

func join(ctx Context, names []string) string {
	var parts []string
	for _, name := range names {
		w, ok := widgets[name]
		if !ok {
			continue
		}
		if s := w(ctx); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------- gozellij's own fields

func session(c Context) string {
	if c.Service == "" {
		return ""
	}
	if c.Of > 1 && c.Position > 0 {
		return fmt.Sprintf("[%s %d/%d]", c.Service, c.Position, c.Of)
	}
	return "[" + c.Service + "]"
}

func services(c Context) string {
	if c.Services == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d up", c.Running, c.Services)
}

func viewers(c Context) string {
	if c.Viewers <= 1 {
		// One viewer is you. Saying so every time would be noise; two is news.
		return ""
	}
	return fmt.Sprintf("%d viewers", c.Viewers)
}

func logs(c Context) string {
	if c.LogBytes <= 0 {
		return ""
	}
	return "logs " + Bytes(c.LogBytes)
}

// ---------------------------------------------------------------------------- host fields

func uptime(Context) string {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return ""
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return ""
	}
	return "up " + Duration(time.Duration(secs)*time.Second)
}

func loadAverage(Context) string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return ""
	}
	return strings.Join(fields[:3], " ")
}

func cpuCount(Context) string {
	return strconv.Itoa(runtime.NumCPU()) + "cpu"
}

func cpuFreq(Context) string {
	// The governor's current frequency where the kernel exposes it; nothing where it does not,
	// which is the case on plenty of virtual machines.
	b, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq")
	if err != nil {
		return ""
	}
	khz, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || khz <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1fGHz", khz/1000/1000)
}

func memory(Context) string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	var total, available int64
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			available = v * 1024
		}
	}
	if total <= 0 {
		return ""
	}
	// MemAvailable, not MemFree: free counts cache as used and panics people for no reason.
	used := total - available
	return fmt.Sprintf("%s/%s", Bytes(used), Bytes(total))
}

func disk(c Context) string {
	path := c.StateDir
	if path == "" {
		path = "/"
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return ""
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	total := int64(st.Blocks) * int64(st.Bsize)
	if total <= 0 {
		return ""
	}
	return Bytes(free) + " free"
}

func date(c Context) string { return c.now().Format("2006-01-02") }
func clock(c Context) string {
	return c.now().Format("15:04")
}

func (c Context) now() time.Time {
	if c.Now.IsZero() {
		return time.Now()
	}
	return c.Now
}

func release(Context) string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func whoami(Context) string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("LOGNAME")
}

func hostname(Context) string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	// The short name: a status line is not the place for a fully qualified domain.
	if i := strings.Index(h, "."); i > 0 {
		h = h[:i]
	}
	return h
}

// ------------------------------------------------------------------------------- formatting

// Bytes is a size a person reads at a glance.
func Bytes(n int64) string {
	if n <= 0 {
		return "0"
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + "B"
	}
	v := float64(n) / 1024
	for _, unit := range []string{"K", "M", "G"} {
		// Promote on the rounded value: comparing raw bytes against the boundary prints
		// "1024K", a unit that does not exist.
		if v < 1023.95 {
			return fmt.Sprintf("%.1f%s", v, unit)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.1fT", v)
}

// Duration is an uptime in the shape byobu writes it: days and hours, or hours and minutes.
func Duration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
