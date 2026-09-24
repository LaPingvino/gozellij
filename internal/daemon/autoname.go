package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
)

// Naming a tab after what runs in it.
//
// A tab gozellij makes for you starts with a placeholder - new-2, or shell at login - and is
// marked AutoName. Joop's idea, from the first morning of real use: the name should be the first
// tool you run in it, so a row of tabs reads "claude vim htop" rather than "shell new-1 new-2".
//
// Following what you do, but slowly: a tab still wearing its placeholder is named as soon as a
// program has held the foreground for a couple of seconds, and a tab named that way is renamed
// when something else has held it for half a minute and would give it a different name (Joop's
// refinement of "the first tool"). Quick commands - ls, cd, git status - never rename anything;
// Claude Code and then vim in the same repository both say gozellij, so nothing flickers; and at
// the prompt the tab keeps the last name it had rather than turning back into "bash". Any rename
// by a person clears the mark, so a name somebody chose is never overwritten.

// autoNameEvery is how often the tabs waiting for a name are looked at; nameAfter how long a
// program has to hold the foreground to name a tab still wearing its placeholder, and renameAfter
// to rename one already named after something else.
const (
	autoNameEvery = time.Second
	nameAfter     = 2 * time.Second
	renameAfter   = 30 * time.Second
)

// placeholder is whether a tab is still wearing the name it was given before anything ran in it.
var placeholder = regexp.MustCompile(`^(new-[0-9]+|shell)$`)

// sameName is whether name is already base, or base with the suffix that kept it unique.
func sameName(name, base string) bool {
	if name == base {
		return true
	}
	rest, ok := strings.CutPrefix(name, base+"-")
	if !ok || rest == "" {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

// held is which process group a tab's foreground has been, and since when.
type held struct {
	pg    int
	since time.Time
}

// notATool are foreground programs that say nothing about what the tab is for: shells started
// inside the shell, and wrappers whose name is not the program's.
var notATool = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "tcsh": true,
	"csh": true, "login": true, "sudo": true, "doas": true, "su": true, "gozellij": true,
	"env": true, "nohup": true, "time": true,
}

var notNameChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// toolName is the service name a program's comm makes, or "" when it makes none.
func toolName(comm string) string {
	n := strings.ToLower(strings.TrimSpace(comm))
	if notATool[n] {
		return ""
	}
	n = strings.Trim(notNameChars.ReplaceAllString(n, "-"), "-_")
	if len(n) > 32 {
		n = n[:32]
	}
	if n == "" || !(n[0] >= 'a' && n[0] <= 'z' || n[0] >= '0' && n[0] <= '9') {
		return ""
	}
	return n
}

// tabName is what a tab running tool in the directory cwd is called: the project rather than the
// tool, where there is one, because four tabs called claude say nothing and gozellij, website and
// notes say which is which (Joop's refinement).
//
// A project is the git repository cwd is in, named after its top directory, so an editor deep
// inside one is named like the Claude Code at its root. Outside a repository, Claude Code still
// takes its directory's name - that directory is what it works on - unless it is the home directory
// or the root, which name nothing. Everything else is named after the tool: htop in ~ is htop.
func tabName(tool, cwd string) string {
	if cwd == "" {
		return tool
	}
	if root := repoRoot(cwd); root != "" {
		if n := toolName(filepath.Base(root)); n != "" {
			return n
		}
	}
	if tool == "claude" {
		home, _ := os.UserHomeDir()
		if cwd != "/" && filepath.Clean(cwd) != filepath.Clean(home) {
			if n := toolName(filepath.Base(cwd)); n != "" {
				return n
			}
		}
	}
	return tool
}

// repoRoot is the top of the git repository dir is in, or "".
func repoRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if d == "/" || d == "." {
			return ""
		}
	}
}

// autoNamer watches the tabs that name themselves.
func (s *Server) autoNamer() {
	seen := map[string]held{}
	t := time.NewTicker(autoNameEvery)
	defer t.Stop()
	for now := range t.C {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		s.autoNameOnce(seen, now)
	}
}

func (s *Server) autoNameOnce(seen map[string]held, now time.Time) {
	list := s.fab.List()
	taken := make(map[string]bool, len(list))
	for _, st := range list {
		taken[st.Service] = true
	}
	for name := range seen {
		if !taken[name] {
			delete(seen, name)
		}
	}
	for _, st := range list {
		name := st.Service
		def, err := s.fab.Definition(name)
		if err != nil || !def.AutoName || !st.Live() {
			delete(seen, name)
			continue
		}
		p, err := s.fab.Process(name)
		if err != nil || p == nil {
			delete(seen, name)
			continue
		}
		pg, ok := p.Foreground()
		if !ok || pg == p.Pid() {
			delete(seen, name) // at its prompt: it keeps the name it has
			continue
		}
		h, known := seen[name]
		if !known || h.pg != pg {
			seen[name] = held{pg: pg, since: now}
			continue
		}
		wait := renameAfter
		if placeholder.MatchString(name) {
			wait = nameAfter
		}
		if now.Sub(h.since) < wait {
			continue
		}
		comm, err := os.ReadFile("/proc/" + strconv.Itoa(pg) + "/comm")
		if err != nil {
			continue
		}
		tool := toolName(string(comm))
		if tool == "" {
			continue
		}
		cwd, _ := os.Readlink("/proc/" + strconv.Itoa(pg) + "/cwd")
		base := tabName(tool, cwd)
		if sameName(name, base) {
			continue
		}
		to := base
		for i := 2; taken[to]; i++ {
			to = base + "-" + strconv.Itoa(i)
		}
		if err := s.fab.Rename(name, to); err != nil {
			var partial *fabric.RenameLogError
			if !errors.As(err, &partial) {
				s.log.Warn("could not name a tab after what runs in it", "service", name, "to", to, "err", err)
				continue
			}
		}
		// A rename clears the mark, because a person's rename is final. This one was not a
		// person's: put it back, so the tab goes on following what is done in it.
		if _, err := s.fab.Update(to, func(d *fabric.Service) { d.AutoName = true }); err != nil {
			s.log.Warn("named a tab but could not keep it naming itself", "service", to, "err", err)
		}
		s.log.Info("named a tab after what runs in it", "was", name, "now", to)
		delete(seen, name)
		seen[to] = held{pg: pg, since: now}
		delete(taken, name)
		taken[to] = true
	}
}
