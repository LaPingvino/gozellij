package status

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is which widgets go where, and whether the line is drawn at all.
type Config struct {
	// Where the line is drawn.
	Where Placement
	// Left and Right are widget names, in order.
	Left, Right []string
	// Every is how often the line is redrawn.
	Every time.Duration
	// Problems are the things in the file that were not understood. They travel with the config
	// rather than replacing it: one misspelt widget must not cost you the other eleven, and must
	// not vanish either.
	Problems []string
}

// Placement is where the status line goes.
type Placement string

const (
	// Bottom reserves the last row of the terminal. The default, and the byobu-shaped answer.
	Bottom Placement = "bottom"
	// Title puts it in the terminal's title bar, which no program can draw over.
	Title Placement = "title"
	// Off draws nothing.
	Off Placement = "off"
)

// DefaultLeft and DefaultRight follow byobu's own tmux defaults, minus the widgets byobu ships
// disabled (network, disk_io, temperature, processes) and plus the things only this program knows:
// which service you are looking at, how many are up, and - first, because it is what somebody
// meeting this for the first time actually needs - that Ctrl-] is a key and Ctrl-] ? explains it.
var (
	DefaultLeft  = []string{"keys", "session", "services"}
	DefaultRight = []string{"uptime", "load_average", "cpu_count", "memory", "disk", "date", "time"}
)

// DefaultEvery is how often the line is redrawn. Two seconds: fast enough that a load spike is
// visible, slow enough that it is not what is causing it.
const DefaultEvery = 2 * time.Second

// MinEvery is the fastest refresh that will be honoured.
//
// Each redraw dials the daemon, so `every=1ms` is a thousand connections a second per attached
// terminal - a status line that becomes the load it is reporting. Accepting the number and
// ignoring it would be the silent kind of wrong, so it is clamped and said out loud.
const MinEvery = 250 * time.Millisecond

// DefaultConfig is what you get having configured nothing.
func DefaultConfig() Config {
	return Config{
		Where: Bottom,
		Left:  append([]string(nil), DefaultLeft...),
		Right: append([]string(nil), DefaultRight...),
		Every: DefaultEvery,
	}
}

// ConfigPath is where the status configuration lives.
func ConfigPath() string {
	if p := os.Getenv("GOZELLIJ_STATUS_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "gozellij", "status")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "gozellij", "status")
	}
	return ""
}

// Load reads the configuration, falling back to the defaults for anything it does not find.
//
// A missing file is not a problem - it is the normal case - but a file that exists and says
// something unreadable is, and that is reported rather than quietly ignored.
func Load() Config {
	cfg := DefaultConfig()
	path := ConfigPath()
	if path == "" {
		return cfg
	}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			cfg.Problems = append(cfg.Problems, fmt.Sprintf("cannot read %s: %v", path, err))
		}
		return cfg
	}
	defer f.Close()

	return parse(f, cfg, path)
}

func parse(r io.Reader, cfg Config, path string) Config {
	scan := bufio.NewScanner(r)
	line := 0
	for scan.Scan() {
		line++
		text := strings.TrimSpace(scan.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			cfg.Problems = append(cfg.Problems, fmt.Sprintf("%s:%d: %q is not key=value", path, line, text))
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)

		switch key {
		case "where":
			switch Placement(value) {
			case Bottom, Title, Off:
				cfg.Where = Placement(value)
			default:
				cfg.Problems = append(cfg.Problems,
					fmt.Sprintf("%s:%d: where=%q; it takes bottom, title or off", path, line, value))
			}
		case "left":
			cfg.Left, cfg.Problems = fields(value, path, line, cfg.Problems)
		case "right":
			cfg.Right, cfg.Problems = fields(value, path, line, cfg.Problems)
		case "every":
			d, err := time.ParseDuration(value)
			if err != nil || d <= 0 {
				cfg.Problems = append(cfg.Problems,
					fmt.Sprintf("%s:%d: every=%q; it takes a duration like 2s", path, line, value))
				break
			}
			if d < MinEvery {
				cfg.Problems = append(cfg.Problems,
					fmt.Sprintf("%s:%d: every=%q is faster than %s, which is the limit: each redraw "+
						"asks the daemon, so this would make the status line the load it reports",
						path, line, value, MinEvery))
				d = MinEvery
			}
			cfg.Every = d
		default:
			cfg.Problems = append(cfg.Problems, fmt.Sprintf("%s:%d: unknown setting %q", path, line, key))
		}
	}
	if err := scan.Err(); err != nil {
		cfg.Problems = append(cfg.Problems, fmt.Sprintf("%s: %v", path, err))
	}

	// A line with no widgets in it is a row of the terminal given up for a blank bar. Somebody who
	// wants no status line means where=off; somebody who has emptied both lists by accident should
	// hear about it rather than wonder what the empty stripe along the bottom is.
	if cfg.Where != Off && len(cfg.Left) == 0 && len(cfg.Right) == 0 {
		cfg.Problems = append(cfg.Problems, fmt.Sprintf(
			"%s: no widgets left in either list, so there is nothing to show; treating it as where=off", path))
		cfg.Where = Off
	}
	return cfg
}

// fields splits a widget list, dropping the ones commented out with a leading # as byobu does, and
// naming the ones it does not recognise.
func fields(value, path string, line int, problems []string) ([]string, []string) {
	var out []string
	for _, name := range strings.Fields(value) {
		if strings.HasPrefix(name, "#") {
			continue // byobu's way of switching one off without deleting it
		}
		if !Known(name) {
			problems = append(problems, fmt.Sprintf("%s:%d: no widget called %q (try: %s)",
				path, line, name, strings.Join(Names(), " ")))
			continue
		}
		out = append(out, name)
	}
	return out, problems
}

// Example is a configuration file with the defaults written out, for `gozellij stats -example`.
func Example() string {
	return fmt.Sprintf(`# %s
#
# Where the status line goes: bottom, title or off.
#
#   bottom  the last row of the terminal, byobu-style. A full-screen program that
#           sets its own scrolling region will draw over it until it exits.
#   title   the terminal's title bar. Nothing can draw over it, and it survives
#           vim and top - but you only see it if your terminal shows titles.
#   off     nothing.
where=%s

# Widgets, in order, as byobu names them. A leading # switches one off without
# deleting it. Everything available:
#
#   %s
left="%s"
right="%s"

# How often to redraw.
every=%s
`, ConfigPath(), Bottom, strings.Join(Names(), " "),
		strings.Join(DefaultLeft, " "), strings.Join(DefaultRight, " "), DefaultEvery)
}
