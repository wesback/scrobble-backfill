package lastfm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wesback/scrobble-backfill/internal/journal"
	"github.com/wesback/scrobble-backfill/internal/spotify"
)

// VerificationResult reports whether one journaled submission is present in
// the current Last.fm history.
type VerificationResult struct {
	Submission journal.Submission
	Status     ComparisonStatus
	Confidence ComparisonConfidence
	Scrobble   *Scrobble
}

// VerificationSummary contains the result counts and the exact history
// interval used for a journal run.
type VerificationSummary struct {
	FromUTC            time.Time
	ToUTC              time.Time
	TimestampTolerance time.Duration
	Submitted          int
	Confirmed          int
	Missing            int
}

// VerifySubmissions reads bounded Last.fm history and matches it against
// journaled submissions using the same metadata, timestamp, and variant
// matching contract as comparison. It never writes to Last.fm or the journal.
func VerifySubmissions(
	ctx context.Context,
	client *Client,
	profile AuthenticatedProfile,
	submissions []journal.Submission,
) (VerificationSummary, []VerificationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(submissions) == 0 {
		return VerificationSummary{}, []VerificationResult{}, nil
	}
	if client == nil {
		return VerificationSummary{}, nil, errors.New("Last.fm verification: client is unavailable")
	}

	start, end, err := submissionBounds(submissions)
	if err != nil {
		return VerificationSummary{}, nil, err
	}
	summary := VerificationSummary{
		FromUTC:            start,
		ToUTC:              end,
		TimestampTolerance: DefaultTimestampTolerance,
		Submitted:          len(submissions),
	}
	candidates := make([]*comparisonCandidate, len(submissions))
	candidatesByKey := make(map[string][]*comparisonCandidate, len(submissions))
	for index, submission := range submissions {
		candidate := &comparisonCandidate{
			play:  journalPlay(submission),
			index: index,
		}
		candidates[index] = candidate
		key := exactComparisonKey(submission.Artist, submission.Track)
		candidatesByKey[key] = append(candidatesByKey[key], candidate)
	}
	matcher := comparisonMatcher{
		candidatesByKey: candidatesByKey,
		candidates:      candidates,
		tolerance:       DefaultTimestampTolerance,
	}

	if err := ReadHistory(ctx, client, profile, start, end, func(scrobble Scrobble) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		matcher.Assign(&historyRecord{scrobble: scrobble})
		return nil
	}); err != nil {
		return summary, nil, fmt.Errorf("Last.fm verification: read history: %w", err)
	}

	results := make([]VerificationResult, len(candidates))
	for index, candidate := range candidates {
		result := VerificationResult{
			Submission: submissions[index],
			Status:     ComparisonStatusMissing,
			Confidence: candidate.confidence,
		}
		if result.Confidence == "" {
			result.Confidence = ComparisonConfidenceLow
		}
		if candidate.matchedRecord != nil {
			scrobble := candidate.matchedRecord.scrobble
			result.Scrobble = &scrobble
		}
		if candidate.matchedRecord != nil && candidate.confidence != ComparisonConfidenceLow {
			result.Status = ComparisonStatusMatched
			summary.Confirmed++
		} else {
			summary.Missing++
		}
		results[index] = result
	}
	return summary, results, nil
}

func submissionBounds(submissions []journal.Submission) (time.Time, time.Time, error) {
	var start, end time.Time
	for index, submission := range submissions {
		timestamp := submission.Timestamp.UTC()
		if timestamp.IsZero() {
			return time.Time{}, time.Time{}, fmt.Errorf("Last.fm verification: journal submission %d timestamp is zero", index+1)
		}
		// Last.fm history has one-second timestamp precision, matching the
		// timestamp sent by track.scrobble.
		timestamp = time.Unix(timestamp.Unix(), 0).UTC()
		if start.IsZero() || timestamp.Before(start) {
			start = timestamp
		}
		if end.IsZero() || timestamp.After(end) {
			end = timestamp
		}
	}
	if start.IsZero() || end.IsZero() {
		return time.Time{}, time.Time{}, errors.New("Last.fm verification: journal submission timestamps are empty")
	}
	return start, end, nil
}

func journalPlay(submission journal.Submission) spotify.Play {
	return spotify.Play{
		ArtistName:   submission.Artist,
		TrackName:    submission.Track,
		Timestamp:    submission.Timestamp.UTC(),
		Milliseconds: int64(submission.Duration) * 1000,
	}
}
