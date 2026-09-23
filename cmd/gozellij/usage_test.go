package main

import (
	"strings"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/status"
)

// The usage text offers a prefix= line as the way to change the key. Offering the key already in
// force reads as an instruction that does nothing, which is what it did for anybody who had
// already followed it.
func TestOtherPrefixIsADifferentKeyThatParses(t *testing.T) {
	for _, current := range []byte{status.DefaultPrefix, 0x02, 0x01} {
		s := otherPrefix(current)
		got, err := status.ParsePrefix(s)
		if err != nil {
			t.Fatalf("with %s in force the usage suggests prefix=%s, which does not parse: %v",
				status.PrefixLabel(current), s, err)
		}
		if got == current {
			t.Errorf("with %s in force the usage suggests prefix=%s - the same key",
				status.PrefixLabel(current), s)
		}
	}
}

// After pacman puts a new client on disk the old daemon keeps running, and ls is where that is
// noticed. Development builds all say "dev" and prove nothing either way, so they say nothing.
func TestVersionSkewIsSaidAndOnlyWhenItIsReal(t *testing.T) {
	cases := []struct{ client, daemon, want string }{
		{"0.r180.aaaaaaa", "0.r175.14ae127", "is older (0.r175.14ae127)"},
		{"0.r175.14ae127", "0.r180.aaaaaaa", "is newer"},
		{"1.2", "1.3", "is 1.3, not 1.2"},
		{"0.r175.14ae127", "0.r175.14ae127", ""},
		{"dev", "0.r175.14ae127", ""},
		{"0.r175.14ae127", "dev", ""},
		{"0.r175.14ae127", "", ""},
	}
	for _, c := range cases {
		got := versionSkew(c.client, c.daemon)
		if c.want == "" {
			if got != "" {
				t.Errorf("client %s, daemon %s: said %q, want nothing", c.client, c.daemon, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) || !strings.Contains(got, "gozellij upgrade") {
			t.Errorf("client %s, daemon %s: said %q, want %q and the way out", c.client, c.daemon, got, c.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"10m", now.Add(-10 * time.Minute)},
		{"1h30m", now.Add(-90 * time.Minute)},
		{"08:15", time.Date(2026, 9, 23, 8, 15, 0, 0, time.UTC)},
		// A clock time still to come today means the last one: yesterday's.
		{"14:05", time.Date(2026, 9, 22, 14, 5, 0, 0, time.UTC)},
		{"2026-09-20T12:00:00Z", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseSince(c.in, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	if _, err := parseSince("yesterday-ish", now); err == nil {
		t.Error("nonsense was accepted")
	}
}
