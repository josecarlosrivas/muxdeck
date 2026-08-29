package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeClampsProgress(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{-0.5, 0}, {0, 0}, {0.42, 0.42}, {1, 1}, {7, 1},
	} {
		s := Status{Progress: &Progress{Value: tc.in}}
		s.Normalize()
		if s.Progress.Value != tc.want {
			t.Errorf("value %v: got %v, want %v", tc.in, s.Progress.Value, tc.want)
		}
	}
}

// Absent and empty are different answers, and the handler's carry-forward
// rule depends on the difference surviving normalization.
func TestNormalizePreservesAbsentAndEmpty(t *testing.T) {
	var absent Status
	absent.Normalize()
	if absent.Progress != nil || absent.Chips != nil {
		t.Errorf("absent: got progress %v chips %v, want both nil", absent.Progress, absent.Chips)
	}

	cleared := Status{Progress: &Progress{}, Chips: []Chip{}}
	cleared.Normalize()
	if cleared.Progress == nil {
		t.Error("empty progress object: got nil, want a zero object")
	}
	if cleared.Chips == nil {
		t.Error("empty chip list: got nil, want an empty list")
	}
}

func TestNormalizeChips(t *testing.T) {
	s := Status{Chips: []Chip{
		{Key: "tests", Value: "12/12", Icon: "check", Color: "accent"},
		{Key: "ctx", Value: "38%", Icon: "not-an-icon", Color: "rebeccapurple"},
		{Key: "dropped", Value: "", Icon: "check"},
		{Key: strings.Repeat("k", 40), Value: strings.Repeat("v", 40)},
	}}
	s.Normalize()
	want := []Chip{
		{Key: "tests", Value: "12/12", Icon: "check", Color: "accent"},
		{Key: "ctx", Value: "38%"},
		{Key: strings.Repeat("k", maxChipText), Value: strings.Repeat("v", maxChipText)},
	}
	if !reflect.DeepEqual(s.Chips, want) {
		t.Errorf("got %#v, want %#v", s.Chips, want)
	}
}

func TestNormalizeCapsChipCount(t *testing.T) {
	chips := make([]Chip, maxChips+4)
	for i := range chips {
		chips[i] = Chip{Key: "k", Value: "v"}
	}
	s := Status{Chips: chips}
	s.Normalize()
	if len(s.Chips) != maxChips {
		t.Errorf("got %d chips, want %d", len(s.Chips), maxChips)
	}
}

// A status whose chips are all unrenderable must arrive as an explicit clear,
// not as "unchanged" — otherwise the row keeps showing what it had.
func TestNormalizeAllChipsDroppedClears(t *testing.T) {
	s := Status{Chips: []Chip{{Key: "a"}, {Key: "b"}}}
	s.Normalize()
	if s.Chips == nil || len(s.Chips) != 0 {
		t.Errorf("got %#v, want an empty non-nil list", s.Chips)
	}
}

func TestTruncateCutsOnRuneBoundary(t *testing.T) {
	if got := truncate("ünïcødé", 3); got != "ünï" {
		t.Errorf("got %q, want %q", got, "ünï")
	}
	if got := truncate("short", 40); got != "short" {
		t.Errorf("got %q, want %q", got, "short")
	}
	s := Status{Note: strings.Repeat("é", maxNote+50)}
	s.Normalize()
	if got := []rune(s.Note); len(got) != maxNote {
		t.Errorf("note: got %d runes, want %d", len(got), maxNote)
	}
	if !strings.HasSuffix(s.Note, "é") {
		t.Errorf("note ends mid-rune: %q", s.Note[len(s.Note)-3:])
	}
}
