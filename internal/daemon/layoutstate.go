package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// A rendered attach's arrangement outlives the attach.
//
// The point of a login multiplexer is that you come back to what you left. Until this, panes were
// rebuilt from nothing on every attach: you split your shell against your logs, detached, came
// back, and had one pane again with no word about where the other went. DESIGN.md's list of what
// Phase 3 is missing named this first.
//
// It is written by the client rather than held by the daemon because a layout is a notion of the
// user interface - which panes this person likes beside each other - and DESIGN.md is explicit
// that the CLI is a replaceable UI over the fabric. The daemon owns processes and their output;
// another UI would have its own idea of what a pane is. It lives in the state directory next to
// the logs, in JSON one line per pane, because state that must survive a reboot goes on disk in
// something a human can read and repair (design rule 5).

// savedPane is one pane of a remembered arrangement.
type savedPane struct {
	Service string  `json:"service"`
	Weight  float64 `json:"weight"`
}

// savedLayout is the arrangement of a rendered attach.
type savedLayout struct {
	// How is "columns" or "rows". A word rather than the bool it comes from, because the file is
	// meant to be edited by hand and `"how": false` says nothing about what it is not.
	How   string      `json:"how"`
	Focus int         `json:"focus"`
	Panes []savedPane `json:"panes"`
}

// LayoutDir is where rendered attaches remember their arrangements.
func LayoutDir() string { return filepath.Join(StateDir(), "layouts") }

// layoutPath is the file for the service an attach was asked for. A layout is keyed by the name
// typed on the command line, so `gozellij attach shell` brings back the panes you last had beside
// your shell rather than whatever was on screen the last time any attach ended.
func layoutPath(service string) (string, error) {
	if service == "" || service == "." || service == ".." || strings.ContainsAny(service, `/\`) {
		return "", fmt.Errorf("service name %q cannot be a layout file name", service)
	}
	return filepath.Join(LayoutDir(), service+".json"), nil
}

// signature is what has to change before the file is written again. Saving happens after every
// command rather than on detach - the usual way a login multiplexer's client ends is the
// connection dropping, not somebody pressing detach, and a save that only runs on a clean exit
// misses exactly the case the feature exists for. Writing a file per keystroke is the price, so
// this is what makes it a file per actual change.
func (l savedLayout) signature() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "%s/%d", l.How, l.Focus)
	for _, p := range l.Panes {
		fmt.Fprintf(b, ";%s=%.2f", p.Service, p.Weight)
	}
	return b.String()
}

func saveLayout(service string, l savedLayout) error {
	path, err := layoutPath(service)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Written whole and renamed into place: a layout half-written by a client that was killed
	// mid-save is worse than no layout at all, because the next attach would refuse to parse it
	// and say so instead of quietly starting fresh.
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadLayout reads a remembered arrangement. A missing file is not an error: it is the ordinary
// case of attaching to something for the first time.
func loadLayout(service string) (savedLayout, bool, error) {
	path, err := layoutPath(service)
	if err != nil {
		return savedLayout{}, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return savedLayout{}, false, nil
	}
	if err != nil {
		return savedLayout{}, false, err
	}
	var l savedLayout
	if err := json.Unmarshal(data, &l); err != nil {
		return savedLayout{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if len(l.Panes) == 0 {
		return savedLayout{}, false, nil
	}
	return l, true, nil
}

// A pane narrower than this is not a pane, it is a margin. Splitting by hand cannot produce one
// without somebody watching it happen; restoring can, on a terminal smaller than the one the
// layout was saved from, with nobody there to undo it. So the restore stops at what fits and says
// which panes it left out.
const (
	minPaneCols = 10
	minPaneRows = 2
)

// paneRoom is how many panes this screen has room for in the given arrangement.
func paneRoom(screen *renderedScreen, how stacked) int {
	cols, rows := screen.ServiceSize()
	n := 1
	if how == inRows {
		n = rows / minPaneRows
	} else {
		n = (cols + 1) / (minPaneCols + 1)
	}
	return max(n, 1)
}

// layoutOf is the current arrangement, in the form that gets written down.
func layoutOf(panes []*livePane, how stacked, focus int) savedLayout {
	l := savedLayout{How: "columns", Focus: focus}
	if how == inRows {
		l.How = "rows"
	}
	for _, p := range panes {
		w := p.weight
		if w <= 0 {
			w = 1
		}
		l.Panes = append(l.Panes, savedPane{Service: p.service, Weight: w})
	}
	return l
}

// restorePanes rebuilds a remembered arrangement around the connection the attach already has.
//
// The name typed on the command line has to appear in it. After enough `n` and `p` it may not -
// the saved panes can all be other services by then - and `gozellij attach shell` that puts up two
// panes with no shell in either has ignored what was asked for. In that case the layout is left on
// disk and this attach starts as one pane, which is what it did before any of this existed.
func restorePanes(socket, service string, first *Client, screen *renderedScreen, l savedLayout) (panes []*livePane, how stacked, focus int, used bool, notes []string) {
	cols, rows := screen.ServiceSize()
	alone := []*livePane{{service: service, client: first, term: grid.New(cols, rows)}}
	how = inColumns
	if l.How == "rows" {
		how = inRows
	}
	at := -1
	for i, p := range l.Panes {
		if p.Service == service {
			at = i
			break
		}
	}
	if at < 0 {
		return alone, inColumns, 0, false, nil
	}
	want := l.Panes
	if room := paneRoom(screen, how); len(want) > room {
		notes = append(notes, fmt.Sprintf("this screen has room for %d of the %d panes you left; the rest are not restored",
			room, len(want)))
		if at >= room {
			// The pane that was asked for is one of the ones that does not fit, so it takes the
			// place of the last that does rather than being the pane that is dropped.
			want = append(append([]savedPane{}, want[:room-1]...), want[at])
			at = room - 1
		} else {
			want = want[:room]
		}
	}
	for i, sp := range want {
		if i == at {
			p := &livePane{service: service, client: first, term: grid.New(cols, rows), weight: sp.Weight}
			panes = append(panes, p)
			continue
		}
		p, err := openPane(socket, sp.Service, screen, len(want))
		if err != nil {
			// A service in the layout that no longer exists is said out loud rather than
			// silently skipped: the arrangement you come back to is then different from the one
			// you left, and not knowing why is worse than the missing pane.
			notes = append(notes, fmt.Sprintf("%s was in this layout but could not be opened: %v", sp.Service, err))
			continue
		}
		p.weight = sp.Weight
		panes = append(panes, p)
	}
	// The keyboard starts in the pane showing what was asked for, whatever the saved focus was.
	// `gozellij attach shell` is a statement about where you want to type, and a restore that
	// puts the cursor in the log pane because that is where it was last time is answering a
	// question nobody asked.
	for i, p := range panes {
		if p.client == first {
			focus = i
			break
		}
	}
	return panes, how, focus, true, notes
}
