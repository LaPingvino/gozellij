package status

import (
	"strings"
	"testing"
	"time"
)

// The tab bar has one job that matters more than the rest: saying which service you are in.
//
// The first version listed every service and let the line truncate from the right, which on a
// narrow terminal cut off the tail - and the tail contained the brackets. Eight services at forty
// columns showed five other names and not yours, which is worse than the "[shell 2/3]" it
// replaced: that at least always said where you were.

func manyServices(current string) Context {
	return Context{
		Service:  current,
		Names:    []string{"shell", "logs", "web", "database", "worker", "cache", "proxy", "metrics"},
		Services: 8, Running: 8,
		Now: time.Date(2026, 9, 18, 15, 4, 0, 0, time.UTC),
	}
}

func TestWhichServiceYouAreInSurvivesANarrowTerminal(t *testing.T) {
	// The opening bracket and the name, not the closing one: at the narrowest width the line is
	// cut mid-word and loses the ']'. "[worker" still says which service you are in, which is the
	// property. Asserting the pair would be asserting something prettier than what matters.
	for _, cols := range []int{120, 80, 60, 40, 30} {
		line := Render(manyServices("worker"), DefaultLeft, DefaultRight, cols)
		if !strings.Contains(line, "[worker") {
			t.Errorf("at %d columns the line does not say which service you are in: %q", cols, line)
		}
	}
}

func TestTheNeighboursAreShownSoNAndPMeanSomething(t *testing.T) {
	// Two on each side: enough to see where next and previous would take you, which is the whole
	// reason to show anything but the current one.
	line := tabs(manyServices("worker"))
	for _, want := range []string{"database", "[worker]", "cache"} {
		if !strings.Contains(line, want) {
			t.Errorf("tabs = %q, missing %q", line, want)
		}
	}
	// And it says there is more in both directions rather than pretending the list ends.
	if !strings.HasPrefix(line, "…") || !strings.HasSuffix(line, "…") {
		t.Errorf("tabs = %q; with services on both sides it should say so", line)
	}
}

func TestAShortListIsShownWhole(t *testing.T) {
	// Nothing is elided when everything fits: the ellipsis is a cost, not decoration.
	c := Context{Service: "b", Names: []string{"a", "b", "c"}, Services: 3, Running: 3}
	if got := tabs(c); got != "a [b] c" {
		t.Errorf("tabs = %q, want %q", got, "a [b] c")
	}
}

func TestTheEndsOfTheListDoNotElideIntoNothing(t *testing.T) {
	// At the first service there is nothing before it, so no leading ellipsis - and the window
	// still shows as many neighbours as it can rather than fewer for being at an edge.
	first := tabs(manyServices("shell"))
	if strings.HasPrefix(first, "…") {
		t.Errorf("at the first service the line claims there is something before it: %q", first)
	}
	last := tabs(manyServices("metrics"))
	if strings.HasSuffix(last, "…") {
		t.Errorf("at the last service the line claims there is something after it: %q", last)
	}
}

func TestOneServiceIsJustItsName(t *testing.T) {
	c := Context{Service: "shell", Names: []string{"shell"}, Services: 1, Running: 1}
	if got := tabs(c); got != "[shell]" {
		t.Errorf("tabs = %q, want [shell]", got)
	}
}
