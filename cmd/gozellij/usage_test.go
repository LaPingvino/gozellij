package main

import (
	"testing"

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
