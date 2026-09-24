package lastfm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/wesback/scrobble-backfill/internal/config"
	"github.com/wesback/scrobble-backfill/internal/journal"
	"github.com/wesback/scrobble-backfill/internal/spotify"
)

const (
	// MaxSubmissionBatchSize is Last.fm's maximum number of tracks in one
	// track.scrobble request.
	MaxSubmissionBatchSize = 50

	// DefaultSubmissionDelay is deliberately conservative. Callers can
	// replace it with a shorter value in tests or a configured value later.
	DefaultSubmissionDelay = config.DefaultBatchDelay

	// DefaultSubmissionMaxRetries is the number of retries after the initial
	// request. The retry delays are delay, delay*2, and so on.
	DefaultSubmissionMaxRetries = 3
)

var (
	errSubmissionClientUnavailable          = errors.New("Last.fm submission client is unavailable")
	errSubmissionJournalUnavailable         = errors.New("Last.fm submission journal is unavailable")
	errSubmissionOutcomeRecorderUnavailable = errors.New("Last.fm submission journal does not support atomic outcome recording")
)

// SubmissionSleeper waits before a submission request. It is a function so
// tests can provide a fake clock without changing production behavior.
type SubmissionSleeper func(context.Context, time.Duration) error

// SubmissionProgress reports the number of submitted batches and the total
// batch count. It is called only after a batch is marked submitted.
type SubmissionProgress func(completed, total int) error

// SubmissionOptions controls pacing, retries, and progress reporting.
type SubmissionOptions struct {
	// BaselineDelay is used between ordinary batch requests and as the first
	// retry delay. Zero selects DefaultSubmissionDelay.
	BaselineDelay time.Duration
	// BaselineDelaySet distinguishes an explicitly selected zero delay from an
	// omitted delay, which uses DefaultSubmissionDelay.
	BaselineDelaySet bool
	// MaxRetries is the number of retries after the initial request. Zero
	// selects DefaultSubmissionMaxRetries.
	MaxRetries int
	// Sleep replaces the real timer in tests. Nil uses a context-aware timer.
	Sleep SubmissionSleeper
	// Progress is called after each batch is successfully marked submitted.
	// Nil disables submission progress reporting.
	Progress SubmissionProgress
}

// SubmissionService is the durable Last.fm write boundary. It owns request
// sizing, pacing, retry handling, and journal transitions; it does not
// choose which plays are missing.
type SubmissionService struct {
	client  *Client
	journal journal.Store
	options SubmissionOptions
}

type submissionResult struct {
	accepted int
	ignored  []ignoredSubmission
}

type ignoredSubmission struct {
	index  int
	code   string
	reason string
}

// NewSubmissionService creates a service backed by client and store.
func NewSubmissionService(client *Client, store journal.Store, options ...SubmissionOptions) *SubmissionService {
	selected := SubmissionOptions{}
	if len(options) > 0 {
		selected = options[0]
	}
	selected = normalizeSubmissionOptions(selected)
	return &SubmissionService{client: client, journal: store, options: selected}
}

// Submit dispatches ordered missing plays for run. Existing submitted batches
// are skipped, while an existing planned batch is resumed rather than planned
// a second time. New batches are journaled immediately before dispatch.
func (s *SubmissionService) Submit(
	ctx context.Context,
	profile AuthenticatedProfile,
	run journal.Run,
	plays []journal.Submission,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.client == nil {
		return errSubmissionClientUnavailable
	}
	if s.journal == nil {
		return errSubmissionJournalUnavailable
	}
	if _, ok := s.journal.(journal.EventBatchRecorder); !ok {
		return errSubmissionOutcomeRecorderUnavailable
	}
	if strings.TrimSpace(profile.Username) == "" {
		return errors.New("Last.fm submission profile username is empty")
	}
	if strings.TrimSpace(profile.SessionKey) == "" {
		return errors.New("Last.fm submission profile session is empty")
	}
	if strings.TrimSpace(run.Profile) == "" || strings.TrimSpace(run.InvocationID) == "" {
		return errors.New("Last.fm submission journal run identity is incomplete")
	}
	batches, err := submissionBatches(plays)
	if err != nil {
		return err
	}
	resume, err := s.journal.Resume(run.Profile, run.InvocationID)
	if err != nil {
		return fmt.Errorf("resume Last.fm submission journal: %w", err)
	}
	if len(resume.Run.Batches) > len(batches) {
		return fmt.Errorf("Last.fm submission journal has %d batches for %d plays", len(resume.Run.Batches), len(plays))
	}
	for index, existing := range resume.Run.Batches {
		if !sameSubmissionPayloads(existing.Payloads, batches[index]) {
			return fmt.Errorf("Last.fm submission journal batch %d does not match ordered missing plays", existing.Sequence)
		}
	}

	completed := len(resume.Submitted)
	previousRequest := completed > 0
	for index := 0; index < len(batches); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		var batch journal.Batch
		if index < len(resume.Run.Batches) {
			batch = resume.Run.Batches[index]
			if batch.State == journal.StateSubmitted {
				continue
			}
			if batch.State != journal.StatePlanned {
				return fmt.Errorf("Last.fm submission journal batch %d has invalid state %q", batch.Sequence, batch.State)
			}
			if hasSubmissionResult(resume.Run, batch.Sequence) {
				if err := s.journal.MarkSubmitted(run.Profile, run.InvocationID, batch.Sequence); err != nil {
					return fmt.Errorf("mark recovered Last.fm submission batch %d submitted: %w", batch.Sequence, err)
				}
				completed++
				if s.options.Progress != nil {
					if err := s.options.Progress(completed, len(batches)); err != nil {
						return fmt.Errorf("report Last.fm submission progress after batch %d: %w", batch.Sequence, err)
					}
				}
				previousRequest = true
				continue
			}
		} else {
			batch, err = s.journal.PlanBatch(run.Profile, run.InvocationID, batches[index])
			if err != nil {
				return fmt.Errorf("plan Last.fm submission batch %d: %w", index+1, err)
			}
		}

		if previousRequest {
			if err := s.wait(ctx, s.options.BaselineDelay); err != nil {
				return fmt.Errorf("wait before Last.fm submission batch %d: %w", batch.Sequence, err)
			}
		}
		result, err := s.dispatch(ctx, profile, batch)
		if err != nil {
			return err
		}
		if err := s.recordSubmissionResult(run, batch, result, profile); err != nil {
			return fmt.Errorf("record Last.fm submission batch %d outcome: %w", batch.Sequence, err)
		}
		if err := s.journal.MarkSubmitted(run.Profile, run.InvocationID, batch.Sequence); err != nil {
			return fmt.Errorf("mark Last.fm submission batch %d submitted: %w", batch.Sequence, err)
		}
		completed++
		if s.options.Progress != nil {
			if err := s.options.Progress(completed, len(batches)); err != nil {
				return fmt.Errorf("report Last.fm submission progress after batch %d: %w", batch.Sequence, err)
			}
		}
		previousRequest = true
	}
	return nil
}

// SubmitSpotify adapts normalized Spotify plays to the journal payload shape.
// It is useful to callers that have just consumed comparison results.
func (s *SubmissionService) SubmitSpotify(
	ctx context.Context,
	profile AuthenticatedProfile,
	run journal.Run,
	plays []spotify.Play,
) error {
	payloads, err := spotifySubmissionPayloads(plays)
	if err != nil {
		return err
	}
	return s.Submit(ctx, profile, run, payloads)
}

// PlanSpotify records the batches that SubmitSpotify would dispatch without
// making any Last.fm request or marking a journal batch submitted. It is used
// by dry-run callers that need durable batch planning.
func (s *SubmissionService) PlanSpotify(ctx context.Context, run journal.Run, plays []spotify.Play) error {
	payloads, err := spotifySubmissionPayloads(plays)
	if err != nil {
		return err
	}
	return s.Plan(ctx, run, payloads)
}

// Plan records all normalized submission batches without dispatching them.
func (s *SubmissionService) Plan(ctx context.Context, run journal.Run, plays []journal.Submission) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.journal == nil {
		return errSubmissionJournalUnavailable
	}
	if strings.TrimSpace(run.Profile) == "" || strings.TrimSpace(run.InvocationID) == "" {
		return errors.New("Last.fm submission journal run identity is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	batches, err := submissionBatches(plays)
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		return nil
	}
	resume, err := s.journal.Resume(run.Profile, run.InvocationID)
	if err != nil {
		return fmt.Errorf("resume Last.fm submission journal: %w", err)
	}
	if len(resume.Run.Batches) > len(batches) {
		return fmt.Errorf("Last.fm submission journal has %d batches for %d plays", len(resume.Run.Batches), len(plays))
	}
	for index, existing := range resume.Run.Batches {
		if !sameSubmissionPayloads(existing.Payloads, batches[index]) {
			return fmt.Errorf("Last.fm submission journal batch %d does not match ordered missing plays", existing.Sequence)
		}
	}
	for index := len(resume.Run.Batches); index < len(batches); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.journal.PlanBatch(run.Profile, run.InvocationID, batches[index]); err != nil {
			return fmt.Errorf("plan Last.fm submission batch %d: %w", index+1, err)
		}
	}
	return nil
}

// SubmitMissing is the functional form of SubmissionService.Submit.
func SubmitMissing(
	ctx context.Context,
	client *Client,
	store journal.Store,
	profile AuthenticatedProfile,
	run journal.Run,
	plays []journal.Submission,
	options ...SubmissionOptions,
) error {
	return NewSubmissionService(client, store, options...).Submit(ctx, profile, run, plays)
}

func spotifySubmissionPayloads(plays []spotify.Play) ([]journal.Submission, error) {
	payloads := make([]journal.Submission, len(plays))
	for index, play := range plays {
		if play.Milliseconds < 0 {
			return nil, fmt.Errorf("Last.fm submission play %d duration must not be negative", index+1)
		}
		payloads[index] = journal.Submission{
			Artist:    play.ArtistName,
			Track:     play.TrackName,
			Album:     play.AlbumName,
			Timestamp: play.Timestamp,
			Duration:  int(play.Milliseconds / 1000),
		}
	}
	return payloads, nil
}

func (s *SubmissionService) dispatch(ctx context.Context, profile AuthenticatedProfile, batch journal.Batch) (submissionResult, error) {
	for attempt := 0; ; attempt++ {
		response, err := s.client.Call(ctx, "track.scrobble", submissionParams(batch.Payloads), profile.SessionKey)
		if err == nil {
			result, parseErr := parseSubmissionResponse(response, len(batch.Payloads))
			if parseErr != nil {
				return submissionResult{}, fmt.Errorf("submit Last.fm batch %d: %w", batch.Sequence, parseErr)
			}
			return result, nil
		}
		if !retryableSubmissionError(err) || attempt >= s.options.MaxRetries {
			return submissionResult{}, fmt.Errorf("submit Last.fm batch %d: %s", batch.Sequence, redactSession(err.Error(), profile.SessionKey))
		}
		delay := retryDelay(s.options.BaselineDelay, attempt)
		if err := s.wait(ctx, delay); err != nil {
			return submissionResult{}, fmt.Errorf("wait to retry Last.fm batch %d: %w", batch.Sequence, err)
		}
	}
}

func (s *SubmissionService) recordSubmissionResult(
	run journal.Run,
	batch journal.Batch,
	result submissionResult,
	profile AuthenticatedProfile,
) error {
	events := make([]journal.Event, 0, len(result.ignored)+1)
	events = append(events, journal.Event{
		Type: "submission.batch.result",
		Data: map[string]string{
			"batch":    strconv.Itoa(batch.Sequence),
			"accepted": strconv.Itoa(result.accepted),
			"ignored":  strconv.Itoa(len(result.ignored)),
			"count":    strconv.Itoa(len(batch.Payloads)),
		},
	})
	for _, ignored := range result.ignored {
		payload := batch.Payloads[ignored.index]
		events = append(events, journal.Event{
			Type: "submission.scrobble.ignored",
			Data: map[string]string{
				"batch":     strconv.Itoa(batch.Sequence),
				"index":     strconv.Itoa(ignored.index + 1),
				"code":      ignored.code,
				"reason":    redactCredentials(ignored.reason, s.client.APIKey, s.client.APISecret, profile.SessionKey),
				"artist":    payload.Artist,
				"track":     payload.Track,
				"timestamp": payload.Timestamp.UTC().Format(time.RFC3339Nano),
			},
		})
	}
	recorder := s.journal.(journal.EventBatchRecorder)
	return recorder.RecordEvents(run.Profile, run.InvocationID, events)
}

func parseSubmissionResponse(response map[string]any, expected int) (submissionResult, error) {
	envelope, ok := response["scrobbles"].(map[string]any)
	if !ok {
		return submissionResult{}, errors.New("Last.fm scrobble response is missing scrobbles")
	}
	attributes, ok := envelope["@attr"].(map[string]any)
	if !ok {
		return submissionResult{}, errors.New("Last.fm scrobble response is missing counts")
	}
	accepted, err := parseResponseCount(attributes["accepted"])
	if err != nil {
		return submissionResult{}, fmt.Errorf("Last.fm scrobble response has invalid accepted count: %w", err)
	}
	ignoredCount, err := parseResponseCount(attributes["ignored"])
	if err != nil {
		return submissionResult{}, fmt.Errorf("Last.fm scrobble response has invalid ignored count: %w", err)
	}
	if accepted > expected || ignoredCount != expected-accepted {
		return submissionResult{}, fmt.Errorf(
			"Last.fm scrobble response counts are inconsistent: accepted %d, ignored %d, submitted %d",
			accepted, ignoredCount, expected,
		)
	}

	var scrobbles []any
	switch items := envelope["scrobble"].(type) {
	case []any:
		scrobbles = items
	case map[string]any:
		scrobbles = []any{items}
	default:
		return submissionResult{}, errors.New("Last.fm scrobble response is missing per-item details")
	}
	if len(scrobbles) != expected {
		return submissionResult{}, fmt.Errorf(
			"Last.fm scrobble response has %d per-item details for %d submitted plays",
			len(scrobbles), expected,
		)
	}

	result := submissionResult{accepted: accepted, ignored: make([]ignoredSubmission, 0, ignoredCount)}
	for index, item := range scrobbles {
		detail, ok := item.(map[string]any)
		if !ok {
			return submissionResult{}, fmt.Errorf("Last.fm scrobble response item %d is malformed", index+1)
		}
		message, ok := detail["ignoredMessage"].(map[string]any)
		if !ok {
			return submissionResult{}, fmt.Errorf("Last.fm scrobble response item %d is missing ignored details", index+1)
		}
		code, err := parseIgnoredCode(message["code"])
		if err != nil {
			return submissionResult{}, fmt.Errorf("Last.fm scrobble response item %d has an invalid ignored code", index+1)
		}
		text, ok := message["#text"].(string)
		if !ok {
			return submissionResult{}, fmt.Errorf("Last.fm scrobble response item %d has an invalid ignored reason", index+1)
		}
		if code != "0" {
			reason := strings.TrimSpace(text)
			if reason == "" {
				return submissionResult{}, fmt.Errorf("Last.fm scrobble response item %d is ignored without a reason", index+1)
			}
			result.ignored = append(result.ignored, ignoredSubmission{index: index, code: code, reason: reason})
		}
	}
	if len(result.ignored) != ignoredCount || accepted != expected-len(result.ignored) {
		return submissionResult{}, fmt.Errorf(
			"Last.fm scrobble response item details are inconsistent with accepted %d and ignored %d counts",
			accepted, ignoredCount,
		)
	}
	return result, nil
}

func parseIgnoredCode(value any) (string, error) {
	switch value := value.(type) {
	case string:
		code := strings.TrimSpace(value)
		if code == "" {
			return "", errors.New("code is empty")
		}
		return code, nil
	case float64:
		code, err := parseResponseCount(value)
		if err != nil {
			return "", err
		}
		return strconv.Itoa(code), nil
	default:
		return "", errors.New("code must be a string or integer")
	}
}

func parseResponseCount(value any) (int, error) {
	var count int64
	var err error
	switch value := value.(type) {
	case string:
		count, err = strconv.ParseInt(value, 10, 0)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= float64(math.MaxInt) || math.Trunc(value) != value {
			return 0, errors.New("count must be a non-negative integer")
		}
		count = int64(value)
	default:
		return 0, errors.New("count must be a string or integer")
	}
	if err != nil || count < 0 || int64(int(count)) != count {
		return 0, errors.New("count must be a non-negative integer")
	}
	return int(count), nil
}

func hasSubmissionResult(run journal.Run, sequence int) bool {
	want := strconv.Itoa(sequence)
	for _, event := range run.Events {
		if event.Type == "submission.batch.result" && event.Data["batch"] == want {
			return true
		}
	}
	return false
}

func (s *SubmissionService) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.options.Sleep(ctx, delay)
}

func normalizeSubmissionOptions(options SubmissionOptions) SubmissionOptions {
	if options.BaselineDelay == 0 && !options.BaselineDelaySet {
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

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDelay(baseline time.Duration, attempt int) time.Duration {
	for i := 0; i < attempt; i++ {
		if baseline > time.Duration(1<<63-1)/2 {
			return time.Duration(1<<63 - 1)
		}
		baseline *= 2
	}
	return baseline
}

func submissionBatches(plays []journal.Submission) ([][]journal.Submission, error) {
	if len(plays) == 0 {
		return nil, nil
	}
	batches := make([][]journal.Submission, 0, (len(plays)+MaxSubmissionBatchSize-1)/MaxSubmissionBatchSize)
	for start := 0; start < len(plays); start += MaxSubmissionBatchSize {
		end := start + MaxSubmissionBatchSize
		if end > len(plays) {
			end = len(plays)
		}
		batch := append([]journal.Submission(nil), plays[start:end]...)
		batches = append(batches, batch)
	}
	return batches, nil
}

func submissionParams(payloads []journal.Submission) map[string]string {
	params := make(map[string]string, len(payloads)*4)
	for index, payload := range payloads {
		params[fmt.Sprintf("artist[%d]", index)] = payload.Artist
		params[fmt.Sprintf("track[%d]", index)] = payload.Track
		params[fmt.Sprintf("timestamp[%d]", index)] = fmt.Sprintf("%d", payload.Timestamp.UTC().Unix())
		if payload.Album != "" {
			params[fmt.Sprintf("album[%d]", index)] = payload.Album
		}
		if payload.Duration > 0 {
			params[fmt.Sprintf("duration[%d]", index)] = fmt.Sprintf("%d", payload.Duration)
		}
	}
	return params
}

func sameSubmissionPayloads(left, right []journal.Submission) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		a, b := left[index], right[index]
		if a.Artist != b.Artist ||
			a.Track != b.Track ||
			a.Album != b.Album ||
			!a.Timestamp.UTC().Equal(b.Timestamp.UTC()) ||
			a.Duration != b.Duration {
			return false
		}
	}
	return true
}

func retryableSubmissionError(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	return httpErr.StatusCode == 429 || (httpErr.StatusCode >= 500 && httpErr.StatusCode < 600)
}

func redactSession(message, sessionKey string) string {
	if sessionKey == "" {
		return message
	}
	return strings.ReplaceAll(message, sessionKey, "[redacted]")
}
