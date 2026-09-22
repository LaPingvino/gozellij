package status

import (
	"strings"
	"testing"
	"time"
)

// Adversarial review of 61e5256.

func TestAdversarialEveryExactlyMinIsAccepted(t *testing.T) {
	cfg := parse(strings.NewReader("every=250ms\n"), DefaultConfig(), "test")
	if cfg.Every != MinEvery || len(cfg.Problems) != 0 {
		t.Errorf("every=250ms: got %v, problems %v", cfg.Every, cfg.Problems)
	}
}

func TestAdversarialDuplicateEveryLastWinsEachClamped(t *testing.T) {
	cfg := parse(strings.NewReader("every=1ms\nevery=5s\n"), DefaultConfig(), "test")
	if cfg.Every != 5*time.Second {
		t.Errorf("every = %v, want 5s (last wins)", cfg.Every)
	}
	if len(cfg.Problems) != 1 {
		t.Errorf("problems = %v, want exactly one (for the 1ms line)", cfg.Problems)
	}
	cfg = parse(strings.NewReader("every=5s\nevery=1ms\n"), DefaultConfig(), "test")
	if cfg.Every != MinEvery {
		t.Errorf("every = %v, want clamped %v", cfg.Every, MinEvery)
	}
}

func TestAdversarialOnlyUnknownWidgetsReportsBoth(t *testing.T) {
	cfg := parse(strings.NewReader("left=\"bogus\"\nright=\"nope\"\n"), DefaultConfig(), "test")
	if cfg.Where != Off {
		t.Errorf("where = %q, want off", cfg.Where)
	}
	t.Logf("problems: %q", cfg.Problems)
	if len(cfg.Problems) != 3 {
		t.Errorf("want 2 unknown-widget problems + 1 off problem, got %d", len(cfg.Problems))
	}
}

func TestAdversarialLeftOnlyRightEmptyIsFine(t *testing.T) {
	cfg := parse(strings.NewReader("left=\"session\"\nright=\"\"\n"), DefaultConfig(), "test")
	if cfg.Where != Bottom || len(cfg.Problems) != 0 {
		t.Errorf("where=%q problems=%v", cfg.Where, cfg.Problems)
	}
}

func TestAdversarialWhereOffWithWidgets(t *testing.T) {
	cfg := parse(strings.NewReader("where=off\nleft=\"session\"\n"), DefaultConfig(), "test")
	if cfg.Where != Off || len(cfg.Problems) != 0 {
		t.Errorf("where=%q problems=%v", cfg.Where, cfg.Problems)
	}
}

func TestAdversarialTitleWithNoWidgetsMessage(t *testing.T) {
	cfg := parse(strings.NewReader("where=title\nleft=\"\"\nright=\"\"\n"), DefaultConfig(), "test")
	t.Logf("where=%q problems=%q", cfg.Where, cfg.Problems)
}

func TestAdversarialClampBypassAttempts(t *testing.T) {
	for _, in := range []string{"every=0.1s\n", "every='100ms'\n", "every=100000ns\n", "every=1ms\nevery=1ms\n", "every=-1s\n", "every=0\n"} {
		cfg := parse(strings.NewReader(in), DefaultConfig(), "test")
		if cfg.Every < MinEvery {
			t.Errorf("%q: every=%v got under the clamp", in, cfg.Every)
		}
		if len(cfg.Problems) == 0 {
			t.Errorf("%q: silently accepted", in)
		}
	}
}
