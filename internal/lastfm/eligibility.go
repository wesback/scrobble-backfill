package lastfm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wesback/scrobble-backfill/internal/spotify"
)

const (
	// MinimumScrobbleMilliseconds is the shortest play Last.fm can scrobble.
	MinimumScrobbleMilliseconds int64 = 30_000
	// FastPathScrobbleMilliseconds is long enough to be eligible without
	// looking up the track duration.
	FastPathScrobbleMilliseconds int64 = 240_000
)

// EligibilityReason is a stable machine-readable explanation for a decision.
type EligibilityReason string

const (
	EligibilityReasonBelowMinimum    EligibilityReason = "below_minimum"
	EligibilityReasonFastPath        EligibilityReason = "fast_path"
	EligibilityReasonAtLeastHalf     EligibilityReason = "at_least_half_duration"
	EligibilityReasonBelowHalf       EligibilityReason = "below_half_duration"
	EligibilityReasonInvalidMetadata EligibilityReason = "invalid_track_metadata"
)

// EligibilityFallbackReason identifies why a duration lookup could not be
// used. It intentionally contains no request or credential details.
type EligibilityFallbackReason string

const (
	EligibilityFallbackMissingDuration   EligibilityFallbackReason = "missing_duration"
	EligibilityFallbackMalformedDuration EligibilityFallbackReason = "malformed_duration"
	EligibilityFallbackLookupFailed      EligibilityFallbackReason = "duration_lookup_failed"
)

// EligibilityDecision is the result of evaluating one normalized Spotify play.
// FallbackReason is set only when a short play could not be evaluated against
// a usable Last.fm duration.
type EligibilityDecision struct {
	Eligible       bool                      `json:"eligible"`
	Reason         EligibilityReason         `json:"reason"`
	FallbackReason EligibilityFallbackReason `json:"fallback_reason,omitempty"`
}

type durationLookup struct {
	duration       int64
	fallbackReason EligibilityFallbackReason
}

// EligibilityLookupFailureHandler receives a duration lookup failure after
// all retry attempts have been exhausted.
type EligibilityLookupFailureHandler func(artist, track string, err error)

// EligibilitySleeper waits before retrying a duration lookup.
type EligibilitySleeper func(context.Context, time.Duration) error

// EligibilityOptions controls duration lookup retries and failure reporting.
type EligibilityOptions struct {
	// BaselineDelay is used for the first retry. Zero selects
	// DefaultSubmissionDelay.
	BaselineDelay time.Duration
	// MaxRetries is the number of retries after the initial lookup. Zero
	// selects DefaultSubmissionMaxRetries.
	MaxRetries int
	// Sleep replaces the real timer in tests. Nil uses a context-aware timer.
	Sleep EligibilitySleeper
	// LookupFailureHandler is called once when a track's lookup permanently
	// fails. Nil disables failure reporting.
	LookupFailureHandler EligibilityLookupFailureHandler
}

// EligibilityEvaluator applies Last.fm's scrobble eligibility rule. Its
// duration cache belongs to one comparison run: create one evaluator for each
// run and discard it when the run ends.
type EligibilityEvaluator struct {
	client  *Client
	profile AuthenticatedProfile
	options EligibilityOptions

	mu        sync.Mutex
	durations map[string]durationLookup
}

// NewEligibilityEvaluator creates an evaluator with an empty run-local cache.
func NewEligibilityEvaluator(client *Client, profile AuthenticatedProfile, options ...EligibilityOptions) *EligibilityEvaluator {
	selected := EligibilityOptions{}
	if len(options) > 0 {
		selected = options[0]
	}
	selected = normalizeEligibilityOptions(selected)
	return &EligibilityEvaluator{
		client:    client,
		profile:   profile,
		options:   selected,
		durations: make(map[string]durationLookup),
	}
}

// Evaluate determines whether play satisfies Last.fm's scrobble eligibility
// rule. Lookup failures are conservative decisions rather than returned
// errors; the machine-readable fallback reason is included in the result.
func (e *EligibilityEvaluator) Evaluate(ctx context.Context, play spotify.Play) (EligibilityDecision, error) {
	if e == nil || e.client == nil {
		return EligibilityDecision{}, errors.New("Last.fm eligibility evaluator: client is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return EligibilityDecision{}, err
	}
	if play.Milliseconds < MinimumScrobbleMilliseconds {
		return EligibilityDecision{
			Reason: EligibilityReasonBelowMinimum,
		}, nil
	}
	if play.Milliseconds >= FastPathScrobbleMilliseconds {
		return EligibilityDecision{
			Eligible: true,
			Reason:   EligibilityReasonFastPath,
		}, nil
	}

	artist := normalizedTrackPart(play.ArtistName)
	track := normalizedTrackPart(play.TrackName)
	if artist == "" || track == "" {
		return EligibilityDecision{
			Reason:         EligibilityReasonInvalidMetadata,
			FallbackReason: EligibilityFallbackMalformedDuration,
		}, nil
	}

	lookup, err := e.duration(ctx, artist, track)
	if err != nil {
		return EligibilityDecision{}, err
	}
	if lookup.fallbackReason != "" {
		return EligibilityDecision{
			Reason:         EligibilityReasonBelowHalf,
			FallbackReason: lookup.fallbackReason,
		}, nil
	}
	if play.Milliseconds >= halfDuration(lookup.duration) {
		return EligibilityDecision{
			Eligible: true,
			Reason:   EligibilityReasonAtLeastHalf,
		}, nil
	}
	return EligibilityDecision{
		Reason: EligibilityReasonBelowHalf,
	}, nil
}

// IsEligible is a convenience wrapper for callers that only need the boolean
// result and the fallback/decision metadata.
func (e *EligibilityEvaluator) IsEligible(ctx context.Context, play spotify.Play) (bool, EligibilityDecision, error) {
	decision, err := e.Evaluate(ctx, play)
	return decision.Eligible, decision, err
}

func (e *EligibilityEvaluator) duration(ctx context.Context, artist, track string) (durationLookup, error) {
	key := canonicalTrackKey(artist, track)

	// Hold the lock through the request so concurrent plays of the same track
	// cannot issue duplicate requests within one comparison run.
	e.mu.Lock()
	if lookup, ok := e.durations[key]; ok {
		e.mu.Unlock()
		return lookup, nil
	}

	for attempt := 0; ; attempt++ {
		payload, err := e.client.Call(ctx, "track.getInfo", map[string]string{
			"artist": artist,
			"track":  track,
		}, e.profile.SessionKey)
		if err == nil {
			duration, reason := parseTrackDuration(payload)
			lookup := durationLookup{duration: duration, fallbackReason: reason}
			e.durations[key] = lookup
			e.mu.Unlock()
			return lookup, nil
		}
		if !retryableSubmissionError(err) || attempt >= e.options.MaxRetries {
			lookup := durationLookup{fallbackReason: EligibilityFallbackLookupFailed}
			e.durations[key] = lookup
			handler := e.options.LookupFailureHandler
			safeErr := errors.New(redactSession(err.Error(), e.profile.SessionKey))
			e.mu.Unlock()
			if handler != nil {
				handler(artist, track, safeErr)
			}
			return lookup, nil
		}
		delay := retryDelay(e.options.BaselineDelay, attempt)
		if delay <= 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
			return durationLookup{}, err
		}
		if err := e.options.Sleep(ctx, delay); err != nil {
			e.mu.Unlock()
			return durationLookup{}, fmt.Errorf("wait to retry Last.fm duration lookup: %w", err)
		}
	}
}

func normalizeEligibilityOptions(options EligibilityOptions) EligibilityOptions {
	if options.BaselineDelay == 0 {
		options.BaselineDelay = DefaultSubmissionDelay
	}
	if options.BaselineDelay < 0 {
		options.BaselineDelay = 0
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = DefaultSubmissionMaxRetries
	}
	if options.MaxRetries < 0 {
		options.MaxRetries = 0
	}
	if options.Sleep == nil {
		options.Sleep = sleepWithContext
	}
	return options
}

func parseTrackDuration(payload map[string]any) (int64, EligibilityFallbackReason) {
	rawTrack, ok := payload["track"]
	if !ok {
		return 0, EligibilityFallbackMissingDuration
	}
	track, ok := rawTrack.(map[string]any)
	if !ok {
		return 0, EligibilityFallbackMalformedDuration
	}
	rawDuration, ok := track["duration"]
	if !ok || rawDuration == nil {
		return 0, EligibilityFallbackMissingDuration
	}

	var duration int64
	switch value := rawDuration.(type) {
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, EligibilityFallbackMalformedDuration
		}
		duration = parsed
	case float64:
		if value < 0 || value > float64(math.MaxInt64) || value != math.Trunc(value) {
			return 0, EligibilityFallbackMalformedDuration
		}
		duration = int64(value)
	case int:
		duration = int64(value)
	case int64:
		duration = value
	default:
		return 0, EligibilityFallbackMalformedDuration
	}
	if duration <= 0 {
		return 0, EligibilityFallbackMissingDuration
	}
	return duration, ""
}

func halfDuration(duration int64) int64 {
	return duration/2 + duration%2
}

func normalizedTrackPart(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func canonicalTrackKey(artist, track string) string {
	artist = strings.ToLower(artist)
	track = strings.ToLower(track)
	return fmt.Sprintf("%d:%s%d:%s", len(artist), artist, len(track), track)
}
