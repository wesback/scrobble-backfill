package lastfm

import (
	"testing"
	"time"
)

func TestRemapTimestampMapsSelFromSelToAndMidpointIntoTargetFromTargetTo(t *testing.T) {
	selFrom := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	selTo := selFrom.Add(10 * time.Hour)
	targetFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	targetTo := targetFrom.Add(2 * time.Hour)

	tests := []struct {
		name      string
		timestamp time.Time
		want      time.Time
	}{
		{name: "selFrom maps to targetFrom", timestamp: selFrom, want: targetFrom},
		{name: "selTo maps to targetTo", timestamp: selTo, want: targetTo},
		{name: "midpoint", timestamp: selFrom.Add(5 * time.Hour), want: targetFrom.Add(time.Hour)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RemapTimestamp(test.timestamp, selFrom, selTo, targetFrom, targetTo)
			if delta := got.Sub(test.want); delta < -time.Second || delta > time.Second {
				t.Fatalf("mapped timestamp = %s, want within one second of %s", got, test.want)
			}
		})
	}
}

func TestRemapTimestampMapsEveryPlayToTargetStartForZeroWidthSelection(t *testing.T) {
	selFrom := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	selTo := selFrom
	targetFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	targetTo := targetFrom.Add(24 * time.Hour)

	for _, timestamp := range []time.Time{selFrom.Add(-time.Hour), selFrom, selFrom.Add(time.Hour)} {
		if got := RemapTimestamp(timestamp, selFrom, selTo, targetFrom, targetTo); !got.Equal(targetFrom) {
			t.Errorf("mapped timestamp for %s = %s, want target start %s", timestamp, got, targetFrom)
		}
	}
}

func TestEnforceMinimumSpacingReturnsInputWhenAlreadySpaced(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	input := []time.Time{start, start.Add(time.Minute), start.Add(2 * time.Minute)}

	got, err := EnforceMinimumSpacing(input, time.Minute, start.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("enforce spacing: %v", err)
	}
	if len(got) != len(input) {
		t.Fatalf("result length = %d, want %d", len(got), len(input))
	}
	for index := range input {
		if !got[index].Equal(input[index]) {
			t.Fatalf("timestamp %d = %s, want unchanged %s", index, got[index], input[index])
		}
	}
	if len(input) > 0 && &got[0] != &input[0] {
		t.Fatal("already-spaced input was copied instead of returned unchanged")
	}
}

func TestEnforceMinimumSpacingRatcheting(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	input := []time.Time{start, start.Add(10 * time.Second), start.Add(15 * time.Second)}
	want := []time.Time{start, start.Add(30 * time.Second), start.Add(time.Minute)}

	got, err := EnforceMinimumSpacing(input, 30*time.Second, start.Add(time.Minute))
	if err != nil {
		t.Fatalf("enforce spacing: %v", err)
	}
	for index := range want {
		if !got[index].Equal(want[index]) {
			t.Fatalf("timestamp %d = %s, want %s", index, got[index], want[index])
		}
	}
	for index := 1; index < len(got); index++ {
		if got[index].Sub(got[index-1]) < 30*time.Second {
			t.Fatalf("timestamps %d and %d are less than 30 seconds apart", index-1, index)
		}
	}
	if !input[1].Equal(start.Add(10 * time.Second)) {
		t.Fatalf("input timestamp was modified to %s", input[1])
	}
}

func TestEnforceMinimumSpacingRejectsWindowOverflowWithoutPartialResult(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	input := []time.Time{start, start.Add(5 * time.Second), start.Add(10 * time.Second)}

	got, err := EnforceMinimumSpacing(input, 30*time.Second, start.Add(45*time.Second))
	if err == nil {
		t.Fatal("expected spacing overflow error")
	}
	if got != nil {
		t.Fatalf("result = %v, want no timestamp slice on error", got)
	}
	if !input[1].Equal(start.Add(5*time.Second)) || !input[2].Equal(start.Add(10*time.Second)) {
		t.Fatalf("input was modified on error: %v", input)
	}
}
