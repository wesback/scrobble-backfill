package lastfm

import (
	"errors"
	"fmt"
	"time"
)

// MinimumScrobbleSpacing is the minimum interval used between imported
// scrobbles when their timestamps are remapped into a shorter window.
const MinimumScrobbleSpacing = 30 * time.Second

// RemapTimestamp linearly maps a timestamp from the selection window to the
// target window. A zero-width selection maps every timestamp to targetFrom.
func RemapTimestamp(timestamp, selectionFrom, selectionTo, targetFrom, targetTo time.Time) time.Time {
	if selectionFrom.Equal(selectionTo) || !timestamp.After(selectionFrom) {
		return targetFrom
	}
	if !timestamp.Before(selectionTo) {
		return targetTo
	}

	selectionLength := selectionTo.Sub(selectionFrom)
	targetLength := targetTo.Sub(targetFrom)
	if selectionLength <= 0 || targetLength <= 0 {
		return targetFrom
	}
	progress := float64(timestamp.Sub(selectionFrom)) / float64(selectionLength)
	return targetFrom.Add(time.Duration(float64(targetLength) * progress))
}

// EnforceMinimumSpacing returns chronological timestamps separated by at
// least minimumGap. If the existing slice already satisfies the constraint,
// it is returned unchanged. On error, no timestamp slice is returned.
func EnforceMinimumSpacing(timestamps []time.Time, minimumGap time.Duration, targetWindowEnd time.Time) ([]time.Time, error) {
	if minimumGap < 0 {
		return nil, errors.New("minimum scrobble spacing must not be negative")
	}
	needsSpacing := false
	for index, timestamp := range timestamps {
		if timestamp.After(targetWindowEnd) {
			return nil, fmt.Errorf("timestamp %d exceeds target window end", index)
		}
		if index > 0 {
			if timestamp.Before(timestamps[index-1]) {
				return nil, errors.New("timestamps must be in chronological order")
			}
			if timestamp.Sub(timestamps[index-1]) < minimumGap {
				needsSpacing = true
			}
		}
	}
	if needsSpacing {
		return ratchetMinimumSpacing(timestamps, minimumGap, targetWindowEnd)
	}
	return timestamps, nil
}

func ratchetMinimumSpacing(timestamps []time.Time, minimumGap time.Duration, targetWindowEnd time.Time) ([]time.Time, error) {
	spaced := append([]time.Time(nil), timestamps...)
	for index := 1; index < len(spaced); index++ {
		earliest := spaced[index-1].Add(minimumGap)
		if spaced[index].Before(earliest) {
			spaced[index] = earliest
		}
		if spaced[index].After(targetWindowEnd) {
			return nil, fmt.Errorf("minimum scrobble spacing exceeds target window end at timestamp %d", index)
		}
	}
	return spaced, nil
}
