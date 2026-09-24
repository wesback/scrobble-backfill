package lastfm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wesback/scrobble-backfill/internal/config"
	"github.com/wesback/scrobble-backfill/internal/spotify"
)

const (
	// DefaultTimestampTolerance is used when a comparison request does not
	// provide a tolerance.
	DefaultTimestampTolerance = config.DefaultTimestampTolerance

	// ExactTimestampWindow is the maximum drift considered near-exact.
	ExactTimestampWindow = 5 * time.Second
)

// ComparisonStatus identifies whether an eligible Spotify play is already
// represented in Last.fm history.
type ComparisonStatus string

const (
	ComparisonStatusMatched ComparisonStatus = "matched"
	ComparisonStatusMissing ComparisonStatus = "missing"
)

// ComparisonConfidence describes the certainty of a comparison result.
type ComparisonConfidence string

const (
	ComparisonConfidenceHigh   ComparisonConfidence = "high"
	ComparisonConfidenceMedium ComparisonConfidence = "medium"
	ComparisonConfidenceLow    ComparisonConfidence = "low"
)

// PlaySource supplies normalized Spotify plays without requiring a caller to
// assemble an export into a slice. The source should call consumer once for
// each play it produces.
type PlaySource func(context.Context, spotify.Consumer) error

// ComparisonConsumer receives one result for each eligible Spotify play.
// Returning an error stops comparison.
type ComparisonConsumer func(ComparisonResult) error

// ComparisonRequest describes one read-only comparison run. From and To are
// interpreted as local calendar dates using Timezone; their clock components
// are ignored.
type ComparisonRequest struct {
	From               time.Time
	To                 time.Time
	Timezone           *time.Location
	TimestampTolerance time.Duration
	// TimestampToleranceSet distinguishes an explicitly supplied zero
	// tolerance from an omitted tolerance, which uses the default.
	TimestampToleranceSet bool
	Plays                 PlaySource
	// DurationLookupFailureHandler reports duration lookup failures after
	// retries are exhausted. Nil disables the warning.
	DurationLookupFailureHandler EligibilityLookupFailureHandler
}

// ComparisonResult is the decision for one eligible Spotify play. A
// low-confidence possible duplicate is deliberately reported as missing.
// Scrobble is populated when a Last.fm record was the candidate considered
// for the decision, including low-confidence possible duplicates.
type ComparisonResult struct {
	Play        spotify.Play         `json:"play"`
	Status      ComparisonStatus     `json:"status"`
	Confidence  ComparisonConfidence `json:"confidence"`
	Scrobble    *Scrobble            `json:"scrobble,omitempty"`
	Eligibility EligibilityDecision  `json:"eligibility"`
}

// ComparisonSummary contains counts from a streaming comparison. It does not
// retain either the Spotify source or Last.fm history.
type ComparisonSummary struct {
	FromUTC            time.Time      `json:"from_utc"`
	ToUTC              time.Time      `json:"to_utc"`
	TimestampTolerance time.Duration  `json:"timestamp_tolerance"`
	Eligible           int            `json:"eligible"`
	Matched            int            `json:"matched"`
	Missing            int            `json:"missing"`
	LastFMScrobbles    int            `json:"lastfm_scrobbles"`
	HighConfidence     int            `json:"high_confidence"`
	MediumConfidence   int            `json:"medium_confidence"`
	LowConfidence      int            `json:"low_confidence"`
	ExcludedByReason   map[string]int `json:"excluded_by_reason,omitempty"`
}

// Compare streams normalized Spotify plays against the profile's bounded
// Last.fm history and emits one result per eligible play. It performs no
// Last.fm writes and does not persist comparison state.
func Compare(
	ctx context.Context,
	client *Client,
	profile AuthenticatedProfile,
	request ComparisonRequest,
	consumer ComparisonConsumer,
) (ComparisonSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if consumer == nil {
		return ComparisonSummary{}, errors.New("Last.fm comparison: result consumer is nil")
	}
	if request.Plays == nil {
		return ComparisonSummary{}, errors.New("Last.fm comparison: Spotify play source is nil")
	}
	if client == nil {
		return ComparisonSummary{}, errors.New("Last.fm comparison: client is unavailable")
	}

	location := request.Timezone
	if location == nil {
		location = time.Local
	}
	start, end, err := comparisonBounds(request.From, request.To, location)
	if err != nil {
		return ComparisonSummary{}, err
	}
	tolerance := request.TimestampTolerance
	if tolerance == 0 && !request.TimestampToleranceSet {
		tolerance = DefaultTimestampTolerance
	}
	if tolerance < 0 {
		return ComparisonSummary{}, errors.New("Last.fm comparison: timestamp tolerance must not be negative")
	}

	summary := ComparisonSummary{
		FromUTC:            start,
		ToUTC:              end,
		TimestampTolerance: tolerance,
		ExcludedByReason:   make(map[string]int),
	}
	evaluator := NewEligibilityEvaluator(client, profile, EligibilityOptions{
		LookupFailureHandler: request.DurationLookupFailureHandler,
	})
	groupsByKey := make(map[string]*comparisonGroup)
	groups := make([]*comparisonGroup, 0)
	historyIndex := make(map[string][]*historyRecord)
	seenHistory := make(map[string]struct{})
	claimedHistory := make(map[string]int)
	historyLoaded := false
	historyOrder := 0
	loadHistory := func() error {
		if historyLoaded {
			return nil
		}
		if err := ReadHistory(ctx, client, profile, start, end, func(scrobble Scrobble) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			identity := scrobbleIdentity(scrobble)
			if _, seen := seenHistory[identity]; !seen {
				seenHistory[identity] = struct{}{}
				summary.LastFMScrobbles++
			}
			key := variantComparisonKey(scrobble.Artist, scrobble.Track)
			historyIndex[key] = append(historyIndex[key], &historyRecord{
				scrobble: scrobble,
				identity: identity,
				order:    historyOrder,
			})
			historyOrder++
			return nil
		}); err != nil {
			return fmt.Errorf("Last.fm comparison: read history: %w", err)
		}
		for _, records := range historyIndex {
			sort.SliceStable(records, func(i, j int) bool {
				return records[i].scrobble.Timestamp.Before(records[j].scrobble.Timestamp)
			})
		}
		historyLoaded = true
		return nil
	}
	latestTimestamp := time.Time{}
	flushGroup := func(group *comparisonGroup) error {
		delete(groupsByKey, group.key)
		for index, candidateGroup := range groups {
			if candidateGroup == group {
				groups = append(groups[:index], groups[index+1:]...)
				break
			}
		}
		if err := loadHistory(); err != nil {
			return err
		}
		return compareGroup(ctx, tolerance, group, historyIndex, &summary, consumer, claimedHistory)
	}
	flushStaleGroups := func(timestamp time.Time) error {
		if latestTimestamp.IsZero() || timestamp.After(latestTimestamp) {
			latestTimestamp = timestamp
		}
		for index := 0; index < len(groups); {
			group := groups[index]
			if latestTimestamp.Sub(group.latestTimestamp) <= comparisonGroupWindow(tolerance) {
				index++
				continue
			}
			if err := flushGroup(group); err != nil {
				return err
			}
		}
		return nil
	}
	sourceErr := request.Plays(ctx, func(play spotify.Play) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		timestamp := play.Timestamp.UTC()
		if timestamp.Before(start) || timestamp.After(end) {
			return nil
		}
		decision, err := evaluator.Evaluate(ctx, play)
		if err != nil {
			return err
		}
		if !decision.Eligible {
			summary.ExcludedByReason[string(decision.Reason)]++
			return nil
		}
		summary.Eligible++
		if err := flushStaleGroups(timestamp); err != nil {
			return err
		}
		groupKey := variantComparisonKey(play.ArtistName, play.TrackName)
		group := groupsByKey[groupKey]
		if group == nil || timestamp.Sub(group.earliestTimestamp) > comparisonGroupWindow(tolerance) ||
			group.latestTimestamp.Sub(timestamp) > comparisonGroupWindow(tolerance) {
			if group != nil {
				if err := flushGroup(group); err != nil {
					return err
				}
			}
			group = &comparisonGroup{
				key:               groupKey,
				candidatesByKey:   make(map[string][]*comparisonCandidate),
				earliestTimestamp: timestamp,
				latestTimestamp:   timestamp,
			}
			groupsByKey[groupKey] = group
			groups = append(groups, group)
		}
		candidate := &comparisonCandidate{
			play:     play,
			decision: decision,
			index:    len(group.candidates),
		}
		group.candidates = append(group.candidates, candidate)
		exactKey := exactComparisonKey(play.ArtistName, play.TrackName)
		group.candidatesByKey[exactKey] = append(group.candidatesByKey[exactKey], candidate)
		if timestamp.Before(group.earliestTimestamp) {
			group.earliestTimestamp = timestamp
		}
		if timestamp.After(group.latestTimestamp) {
			group.latestTimestamp = timestamp
		}
		return nil
	})
	if sourceErr != nil {
		return summary, fmt.Errorf("Last.fm comparison: read Spotify plays: %w", sourceErr)
	}
	for len(groups) > 0 {
		if err := flushGroup(groups[0]); err != nil {
			return summary, fmt.Errorf("Last.fm comparison: finalize results: %w", err)
		}
	}
	return summary, nil
}

// comparisonGroup retains only source plays whose timestamp windows can
// overlap. Once the source advances past that window, the group is compared
// and emitted before more source records are accepted.
type comparisonGroup struct {
	key               string
	earliestTimestamp time.Time
	latestTimestamp   time.Time
	candidates        []*comparisonCandidate
	candidatesByKey   map[string][]*comparisonCandidate
}

func comparisonGroupWindow(tolerance time.Duration) time.Duration {
	return tolerance * 2
}

func compareGroup(
	ctx context.Context,
	tolerance time.Duration,
	group *comparisonGroup,
	historyIndex map[string][]*historyRecord,
	summary *ComparisonSummary,
	consumer ComparisonConsumer,
	claimedHistory map[string]int,
) error {
	matcher := comparisonMatcher{
		candidatesByKey: group.candidatesByKey,
		candidates:      group.candidates,
		tolerance:       tolerance,
	}
	historyOccurrences := make(map[string]int)
	records := historyIndex[group.key]
	earliest := group.earliestTimestamp.Add(-tolerance)
	latest := group.latestTimestamp.Add(tolerance)
	first := sort.Search(len(records), func(index int) bool {
		return !records[index].scrobble.Timestamp.Before(earliest)
	})
	last := sort.Search(len(records), func(index int) bool {
		return records[index].scrobble.Timestamp.After(latest)
	})
	relevantRecords := append([]*historyRecord(nil), records[first:last]...)
	sort.SliceStable(relevantRecords, func(i, j int) bool {
		return relevantRecords[i].order < relevantRecords[j].order
	})
	for _, record := range relevantRecords {
		if err := ctx.Err(); err != nil {
			return err
		}
		occurrence := historyOccurrences[record.identity]
		historyOccurrences[record.identity] = occurrence + 1
		if occurrence < claimedHistory[record.identity] {
			continue
		}
		matcher.Assign(record)
	}

	sort.SliceStable(group.candidates, func(i, j int) bool {
		return group.candidates[i].index < group.candidates[j].index
	})
	for _, candidate := range group.candidates {
		result := ComparisonResult{
			Play:        candidate.play,
			Status:      ComparisonStatusMissing,
			Confidence:  candidate.confidence,
			Eligibility: candidate.decision,
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
			claimedHistory[candidate.matchedRecord.identity]++
		}
		if result.Status == ComparisonStatusMatched {
			summary.Matched++
		} else {
			summary.Missing++
		}

		switch result.Confidence {
		case ComparisonConfidenceHigh:
			summary.HighConfidence++
		case ComparisonConfidenceMedium:
			summary.MediumConfidence++
		case ComparisonConfidenceLow:
			summary.LowConfidence++
		}
		if err := consumer(result); err != nil {
			return fmt.Errorf("Last.fm comparison: consume result: %w", err)
		}
	}
	return nil
}

func scrobbleIdentity(scrobble Scrobble) string {
	// Keep the source spelling in the identity. Matching intentionally folds
	// case and whitespace, but an identity must remain stable when the API
	// returns equivalent records in a different order.
	return fmt.Sprintf(
		"%d:%s\x00%d:%s\x00%d",
		len(scrobble.Artist),
		scrobble.Artist,
		len(scrobble.Track),
		scrobble.Track,
		scrobble.Timestamp.UTC().UnixNano(),
	)
}

// ComparePlays adapts an in-memory set of normalized plays to Compare. It is
// convenient for small callers; large exports should pass a streaming
// PlaySource instead.
func ComparePlays(
	ctx context.Context,
	client *Client,
	profile AuthenticatedProfile,
	plays []spotify.Play,
	from, to time.Time,
	timezone *time.Location,
	tolerance time.Duration,
	consumer ComparisonConsumer,
) (ComparisonSummary, error) {
	return Compare(ctx, client, profile, ComparisonRequest{
		From:                  from,
		To:                    to,
		Timezone:              timezone,
		TimestampTolerance:    tolerance,
		TimestampToleranceSet: true,
		Plays: func(ctx context.Context, consume spotify.Consumer) error {
			for _, play := range plays {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := consume(play); err != nil {
					return err
				}
			}
			return nil
		},
	}, consumer)
}

type comparisonCandidate struct {
	play          spotify.Play
	decision      EligibilityDecision
	index         int
	matchedRecord *historyRecord
	confidence    ComparisonConfidence
}

type historyRecord struct {
	scrobble Scrobble
	identity string
	order    int
}

type comparisonMatcher struct {
	candidatesByKey map[string][]*comparisonCandidate
	candidates      []*comparisonCandidate
	tolerance       time.Duration
}

// Assign adds one history record to the matching graph. If its preferred
// candidate is already occupied, the previous record is recursively moved to
// its next-best candidate. This augmenting-path step makes assignments
// independent of Last.fm page/callback order while retaining only records
// currently paired with eligible plays.
func (m comparisonMatcher) Assign(record *historyRecord) {
	_ = m.assign(record, make(map[*comparisonCandidate]bool))
}

func (m comparisonMatcher) assign(record *historyRecord, visited map[*comparisonCandidate]bool) bool {
	scores := m.scores(record.scrobble)
	for _, score := range scores {
		candidate := score.candidate
		if visited[candidate] {
			continue
		}
		visited[candidate] = true
		if candidate.matchedRecord != nil {
			if !m.assign(candidate.matchedRecord, visited) {
				continue
			}
		}
		candidate.matchedRecord = record
		candidate.confidence = scoreConfidence(scores, score, m.tolerance)
		return true
	}
	return false
}

func (m comparisonMatcher) scores(scrobble Scrobble) []candidateScore {
	exact := m.candidatesByKey[exactComparisonKey(scrobble.Artist, scrobble.Track)]
	scores := comparisonScores(exact, scrobble, m.tolerance, false)
	if len(scores) > 0 {
		return scores
	}
	// A punctuation/parenthetical metadata variant can still be a useful
	// possible duplicate, but it is never safe to mark matched.
	return comparisonScores(m.candidates, scrobble, m.tolerance, true)
}

func comparisonBounds(from, to time.Time, location *time.Location) (time.Time, time.Time, error) {
	if from.IsZero() || to.IsZero() {
		return time.Time{}, time.Time{}, errors.New("Last.fm comparison: from and to dates must be set")
	}
	startLocal := time.Date(from.In(location).Year(), from.In(location).Month(), from.In(location).Day(), 0, 0, 0, 0, location)
	endLocal := time.Date(to.In(location).Year(), to.In(location).Month(), to.In(location).Day(), 0, 0, 0, 0, location).
		AddDate(0, 0, 1).Add(-time.Nanosecond)
	start := startLocal.UTC()
	end := endLocal.UTC()
	if start.After(end) {
		return time.Time{}, time.Time{}, errors.New("Last.fm comparison: from date must not be after to date")
	}
	return start, end, nil
}

func comparisonScores(candidates []*comparisonCandidate, scrobble Scrobble, tolerance time.Duration, variantsOnly bool) []candidateScore {
	eligible := make([]candidateScore, 0, len(candidates))
	for _, candidate := range candidates {
		if variantsOnly && variantComparisonKey(candidate.play.ArtistName, candidate.play.TrackName) != variantComparisonKey(scrobble.Artist, scrobble.Track) {
			continue
		}
		delta := absDuration(candidate.play.Timestamp.UTC().Sub(scrobble.Timestamp.UTC()))
		if delta > tolerance {
			continue
		}
		eligible = append(eligible, candidateScore{
			candidate: candidate,
			delta:     delta,
			variant:   false,
		})
	}
	if variantsOnly {
		for index := range eligible {
			eligible[index].variant = true
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].delta != eligible[j].delta {
			return eligible[i].delta < eligible[j].delta
		}
		if eligible[i].candidate.play.Milliseconds != eligible[j].candidate.play.Milliseconds {
			return eligible[i].candidate.play.Milliseconds > eligible[j].candidate.play.Milliseconds
		}
		return eligible[i].candidate.index < eligible[j].candidate.index
	})
	return eligible
}

type candidateScore struct {
	candidate *comparisonCandidate
	delta     time.Duration
	variant   bool
}

func scoreConfidence(scores []candidateScore, selected candidateScore, tolerance time.Duration) ComparisonConfidence {
	msDisambiguated := false
	for _, score := range scores {
		if score.candidate == selected.candidate {
			continue
		}
		if score.delta == selected.delta && score.candidate.play.Milliseconds != selected.candidate.play.Milliseconds {
			msDisambiguated = true
			break
		}
	}
	confidence := ComparisonConfidenceMedium
	if selected.variant {
		confidence = ComparisonConfidenceLow
	} else if msDisambiguated {
		confidence = ComparisonConfidenceMedium
	} else if selected.delta == 0 {
		confidence = ComparisonConfidenceHigh
	} else if selected.delta == tolerance {
		confidence = ComparisonConfidenceLow
	} else if selected.delta <= ExactTimestampWindow {
		confidence = ComparisonConfidenceHigh
	}
	return confidence
}

func exactComparisonKey(artist, track string) string {
	return normalizedComparisonPart(artist) + "\x00" + normalizedComparisonPart(track)
}

func variantComparisonKey(artist, track string) string {
	return variantComparisonPart(artist) + "\x00" + variantComparisonPart(track)
}

func variantComparisonPart(value string) string {
	var builder strings.Builder
	depth := 0
	for _, r := range value {
		switch r {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth > 0 {
			continue
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r + ('a' - 'A'))
		}
	}
	return builder.String()
}

func normalizedComparisonPart(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
