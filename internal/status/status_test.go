package status

import (
	"strings"
	"testing"
	"time"
)

func ctx() Context {
	return Context{
		Service: "web", Position: 2, Of: 5,
		Running: 3, Services: 5, Viewers: 2, LogBytes: 5 << 20,
		Now: time.Date(2026, 9, 18, 15, 4, 0, 0, time.UTC),
	}
}

func TestRenderPushesTheRightHandFieldsToTheMargin(t *testing.T) {
	line := Render(ctx(), []string{"session"}, []string{"time"}, 40)
	if len(line) != 40 {
		t.Errorf("line is %d wide, want 40: %q", len(line), line)
	}
	if !strings.HasPrefix(line, "[web 2/5]") {
		t.Errorf("left field is not at the left: %q", line)
	}
	if !strings.HasSuffix(line, "15:04") {
		t.Errorf("right field is not at the right margin: %q", line)
	}
}

func TestRenderDropsRightHandFieldsRatherThanOverflowing(t *testing.T) {
	// byobu orders the right-hand fields least to most important, so what goes is the uptime and
	// what stays is the clock. A line that wraps is worse than a line that is shorter.
	wide := Render(ctx(), []string{"session"}, []string{"uptime", "services", "time"}, 200)
	narrow := Render(ctx(), []string{"session"}, []string{"uptime", "services", "time"}, 24)

	if !strings.Contains(wide, "3/5 up") {
		t.Fatalf("precondition: the wide line should have every field: %q", wide)
	}
	if len(narrow) > 24 {
		t.Errorf("the narrow line is %d wide, want at most 24: %q", len(narrow), narrow)
	}
	if !strings.Contains(narrow, "15:04") {
		t.Errorf("the clock was dropped before the less important fields: %q", narrow)
	}
}

func TestAWidgetWithNothingToSaySaysNothing(t *testing.T) {
	// A gap is a better report than an invented number. `viewers` is the clearest case: one
	// viewer is you, and saying so every two seconds is noise.
	if got := Render(Context{Service: "web", Viewers: 1}, nil, []string{"viewers"}, 0); got != "" {
		t.Errorf("viewers rendered %q for a single viewer, want nothing", got)
	}
	if got := Render(Context{Service: "web", Viewers: 3}, nil, []string{"viewers"}, 0); got != "3 viewers" {
		t.Errorf("viewers = %q, want 3 viewers", got)
	}
	if got := Render(Context{}, nil, []string{"session", "services", "logs"}, 0); got != "" {
		t.Errorf("widgets with no data rendered %q, want nothing at all", got)
	}
}

func TestUnknownWidgetsAreSkippedNotRendered(t *testing.T) {
	if got := Render(ctx(), []string{"session", "no_such_widget"}, nil, 0); got != "[web 2/5]" {
		t.Errorf("render = %q; an unknown name must not appear in the line", got)
	}
}

func TestBytesPromotesOnTheRoundedValue(t *testing.T) {
	cases := map[int64]string{
		0: "0", 512: "512B", 2048: "2.0K",
		1024*1024 - 1: "1.0M", 1 << 20: "1.0M", 1<<30 - 1: "1.0G",
	}
	for n, want := range cases {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDurationReadsLikeAnUptime(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:       "0m",
		90 * time.Minute:       "1h30m",
		50 * time.Hour:         "2d2h",
		(24*6 + 4) * time.Hour: "6d4h",
	}
	for d, want := range cases {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestConfigFollowsByobusShape(t *testing.T) {
	cfg := parse(strings.NewReader(`
# a comment
where=title
left="session #services"
right="time"
every=5s
`), DefaultConfig(), "test")

	if cfg.Where != Title {
		t.Errorf("where = %q, want title", cfg.Where)
	}
	// A leading # switches a widget off without deleting it, as byobu does.
	if len(cfg.Left) != 1 || cfg.Left[0] != "session" {
		t.Errorf("left = %v, want [session] with services commented out", cfg.Left)
	}
	if cfg.Every != 5*time.Second {
		t.Errorf("every = %v, want 5s", cfg.Every)
	}
	if len(cfg.Problems) != 0 {
		t.Errorf("problems on a valid file: %v", cfg.Problems)
	}
}

func TestConfigReportsWhatItCouldNotUnderstandAndKeepsTheRest(t *testing.T) {
	cfg := parse(strings.NewReader(`
where=sideways
left="session widgetthatisnotreal"
nonsense
every=fortnight
right="time"
`), DefaultConfig(), "test")

	if len(cfg.Problems) != 4 {
		t.Errorf("problems = %v, want one for each of the four bad lines", cfg.Problems)
	}
	// One misspelt widget must not cost you the other eleven.
	if len(cfg.Left) != 1 || cfg.Left[0] != "session" {
		t.Errorf("left = %v, want the good widget kept", cfg.Left)
	}
	if len(cfg.Right) != 1 || cfg.Right[0] != "time" {
		t.Errorf("right = %v, want the later good line still applied", cfg.Right)
	}
	if cfg.Where != Bottom {
		t.Errorf("where = %q; an unusable value must leave the default in place", cfg.Where)
	}
}

func TestTheExampleConfigIsItselfValid(t *testing.T) {
	cfg := parse(strings.NewReader(Example()), DefaultConfig(), "example")
	if len(cfg.Problems) != 0 {
		t.Errorf("the example configuration does not parse cleanly: %v", cfg.Problems)
	}
}

func TestAnImpossiblyFastRefreshIsClampedAndSaidSo(t *testing.T) {
	// Each redraw dials the daemon. Accepting `every=1ms` and honouring it makes the status line
	// the load it is reporting; accepting it and quietly ignoring it is the other kind of wrong.
	cfg := parse(strings.NewReader("every=1ms\n"), DefaultConfig(), "test")

	if cfg.Every != MinEvery {
		t.Errorf("every = %v, want it clamped to %v", cfg.Every, MinEvery)
	}
	if len(cfg.Problems) != 1 {
		t.Errorf("problems = %v, want one saying it was clamped", cfg.Problems)
	}
	// And a sane value is left alone.
	if cfg := parse(strings.NewReader("every=5s\n"), DefaultConfig(), "test"); cfg.Every != 5*time.Second {
		t.Errorf("every = %v, want 5s untouched", cfg.Every)
	}
}

func TestALineWithNoWidgetsIsTreatedAsOff(t *testing.T) {
	// Otherwise it is a row of the terminal given up for a blank stripe, with nothing anywhere
	// saying why.
	cfg := parse(strings.NewReader("left=\"\"\nright=\"\"\n"), DefaultConfig(), "test")

	if cfg.Where != Off {
		t.Errorf("where = %q, want off when there is nothing to draw", cfg.Where)
	}
	if len(cfg.Problems) != 1 {
		t.Errorf("problems = %v, want one explaining the empty line", cfg.Problems)
	}

	// Emptying them by commenting every widget out counts too - that is the likeliest way to
	// arrive here by accident.
	cfg = parse(strings.NewReader("left=\"#session\"\nright=\"#time\"\n"), DefaultConfig(), "test")
	if cfg.Where != Off {
		t.Errorf("where = %q after commenting out every widget, want off", cfg.Where)
	}

	// But an explicit where=off is not an error worth reporting: it is what was asked for.
	cfg = parse(strings.NewReader("where=off\nleft=\"\"\nright=\"\"\n"), DefaultConfig(), "test")
	if len(cfg.Problems) != 0 {
		t.Errorf("problems = %v for an explicit where=off, want none", cfg.Problems)
	}
}

func TestTrimmingDropsWholeWidgetsNotWords(t *testing.T) {
	// Several widgets contain spaces - the load average is three numbers - so trimming by word
	// left "0.31 0.28" on the line. That does not look like a truncated load average; it looks
	// like a load average, which is worse than not showing one.
	c := ctx()
	full := Render(c, []string{"session"}, []string{"services", "viewers", "time"}, 200)
	if !strings.Contains(full, "3/5 up") || !strings.Contains(full, "2 viewers") {
		t.Fatalf("precondition: the wide line should hold every field: %q", full)
	}

	// Narrow enough that the first right-hand widget cannot fit.
	narrow := Render(c, []string{"session"}, []string{"services", "viewers", "time"}, 30)
	if strings.Contains(narrow, "3/5") || strings.Contains(narrow, "/5 up") {
		t.Errorf("a fragment of the services widget survived trimming: %q", narrow)
	}
	if !strings.Contains(narrow, "15:04") {
		t.Errorf("the most important field was dropped before the least: %q", narrow)
	}
}

func TestNeededAndDroppedSayWhatWillNotFit(t *testing.T) {
	c := ctx()
	left := []string{"session"}
	right := []string{"services", "viewers", "time"}

	needed := Needed(c, left, right)
	if needed <= 0 {
		t.Fatalf("Needed = %d, want the full width of the line", needed)
	}
	// At the width it asks for, nothing is lost.
	if lost := Dropped(c, left, right, needed); len(lost) != 0 {
		t.Errorf("Dropped = %v at exactly the needed width, want nothing", lost)
	}
	// One column short, and the first right-hand widget goes - named, so it can be edited.
	lost := Dropped(c, left, right, needed-1)
	if len(lost) != 1 || lost[0] != "services" {
		t.Errorf("Dropped = %v one column short, want [services]", lost)
	}
	// And the names come back in the order they are lost.
	if lost := Dropped(c, left, right, 20); len(lost) < 2 || lost[0] != "services" || lost[1] != "viewers" {
		t.Errorf("Dropped = %v at 20 columns, want services then viewers", lost)
	}
	// A width of zero means "no limit", not "everything is dropped".
	if lost := Dropped(c, left, right, 0); len(lost) != 0 {
		t.Errorf("Dropped = %v with no width given, want nothing", lost)
	}
}
