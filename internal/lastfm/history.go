package lastfm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wesback/scrobble-backfill/internal/config"
)

// AuthenticatedProfile is the resolved Last.fm identity and session used by
// authenticated profile requests. SessionKey is never included in a
// normalized record or request error.
type AuthenticatedProfile struct {
	Username   string
	SessionKey string
}

// Scrobble is the normalized representation of one dated Last.fm scrobble.
type Scrobble struct {
	Artist    string
	Track     string
	Timestamp time.Time
}

// HistoryConsumer receives one scrobble at a time. Returning an error stops
// history retrieval.
type HistoryConsumer func(Scrobble) error

const (
	// DefaultHistoryDelay is the baseline delay between retry attempts.
	DefaultHistoryDelay = config.DefaultBatchDelay

	// DefaultHistoryMaxRetries is the number of retries after the initial
	// page request.
	DefaultHistoryMaxRetries = 3
)

// HistorySleeper waits before retrying a history page request. It is a
// function so tests can provide a fake clock without changing production
// behavior.
type HistorySleeper func(context.Context, time.Duration) error

// HistoryOptions controls retry behavior for history page requests.
type HistoryOptions struct {
	// BaselineDelay is the first retry delay. Zero selects
	// DefaultHistoryDelay unless BaselineDelaySet is true.
	BaselineDelay time.Duration
	// BaselineDelaySet distinguishes an explicitly selected zero delay from
	// an omitted delay, which uses DefaultHistoryDelay.
	BaselineDelaySet bool
	// MaxRetries is the number of retries after the initial request. Zero
	// selects DefaultHistoryMaxRetries.
	MaxRetries int
	// Sleep replaces the real timer in tests. Nil uses a context-aware timer.
	Sleep HistorySleeper
}

// HistoryReader reads a profile's bounded Last.fm history through Client.
type HistoryReader struct {
	client  *Client
	options HistoryOptions
}

// NewHistoryReader creates a history reader backed by client.
func NewHistoryReader(client *Client, options ...HistoryOptions) *HistoryReader {
	selected := HistoryOptions{}
	if len(options) > 0 {
		selected = options[0]
	}
	return &HistoryReader{client: client, options: normalizeHistoryOptions(selected)}
}

// ReadHistory streams dated scrobbles for profile in the inclusive interval
// [start, end]. The Last.fm API is queried one page at a time.
func ReadHistory(
	ctx context.Context,
	client *Client,
	profile AuthenticatedProfile,
	start, end time.Time,
	consumer HistoryConsumer,
) error {
	return NewHistoryReader(client).Read(ctx, profile, start, end, consumer)
}

// ReadHistory streams dated scrobbles for profile in the inclusive interval
// [start, end]. The Last.fm API is queried one page at a time.
func (c *Client) ReadHistory(
	ctx context.Context,
	profile AuthenticatedProfile,
	start, end time.Time,
	consumer HistoryConsumer,
) error {
	return NewHistoryReader(c).Read(ctx, profile, start, end, consumer)
}

// Read streams dated scrobbles for profile in the inclusive interval
// [start, end]. Timestamps and request bounds are normalized to UTC.
func (r *HistoryReader) Read(
	ctx context.Context,
	profile AuthenticatedProfile,
	start, end time.Time,
	consumer HistoryConsumer,
) error {
	if r == nil || r.client == nil {
		return errors.New("Last.fm history request: client is unavailable")
	}
	if strings.TrimSpace(profile.Username) == "" {
		return errors.New("Last.fm history request: authenticated profile username is empty")
	}
	if strings.TrimSpace(profile.SessionKey) == "" {
		return errors.New("Last.fm history request: authenticated profile session is empty")
	}
	if consumer == nil {
		return errors.New("Last.fm history request: consumer is nil")
	}

	start = start.UTC()
	end = end.UTC()
	if start.IsZero() || end.IsZero() {
		return errors.New("Last.fm history request: start and end must be set")
	}
	if start.After(end) {
		return errors.New("Last.fm history request: start must not be after end")
	}

	for page := 1; ; page++ {
		var payload map[string]any
		for attempt := 0; ; attempt++ {
			var err error
			payload, err = r.client.Call(ctx, "user.getRecentTracks", map[string]string{
				"user": profile.Username,
				"from": strconv.FormatInt(start.Unix(), 10),
				"to":   strconv.FormatInt(end.Unix(), 10),
				"page": strconv.Itoa(page),
			}, profile.SessionKey)
			if err == nil {
				break
			}
			if !retryableSubmissionError(err) || attempt >= r.options.MaxRetries {
				return historyRequestError(page, profile.SessionKey, err)
			}
			delay := retryDelay(r.options.BaselineDelay, attempt)
			if err := r.wait(ctx, delay); err != nil {
				return fmt.Errorf("wait to retry Last.fm history page %d: %w", page, err)
			}
		}

		tracks, totalPages, err := parseHistoryPage(payload)
		if err != nil {
			return historyRequestError(page, profile.SessionKey, err)
		}
		for _, track := range tracks {
			if track.Timestamp.Before(start) || track.Timestamp.After(end) {
				continue
			}
			if err := consumer(track); err != nil {
				return fmt.Errorf("consume Last.fm history record on page %d: %w", page, err)
			}
		}
		if page >= totalPages {
			return nil
		}
	}
}

func (r *HistoryReader) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.options.Sleep(ctx, delay)
}

func normalizeHistoryOptions(options HistoryOptions) HistoryOptions {
	if options.BaselineDelay == 0 && !options.BaselineDelaySet {
		options.BaselineDelay = DefaultHistoryDelay
	}
	if options.BaselineDelay < 0 {
		options.BaselineDelay = 0
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = DefaultHistoryMaxRetries
	}
	if options.MaxRetries < 0 {
		options.MaxRetries = 0
	}
	if options.Sleep == nil {
		options.Sleep = sleepWithContext
	}
	return options
}

func historyRequestError(page int, sessionKey string, err error) error {
	message := err.Error()
	if sessionKey != "" {
		message = strings.ReplaceAll(message, sessionKey, "[redacted]")
	}
	return fmt.Errorf("Last.fm history request page %d: %s", page, message)
}

func parseHistoryPage(payload map[string]any) ([]Scrobble, int, error) {
	recentTracks, ok := payload["recenttracks"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("malformed response: recenttracks is missing")
	}

	totalPages, err := historyInteger(recentTracks, "@attr", "totalPages")
	if err != nil || totalPages < 1 {
		if err == nil {
			err = errors.New("must be at least 1")
		}
		return nil, 0, fmt.Errorf("malformed response: totalPages %w", err)
	}

	rawTracks, ok := recentTracks["track"].([]any)
	if !ok {
		return nil, 0, errors.New("malformed response: track list is missing")
	}
	tracks := make([]Scrobble, 0, len(rawTracks))
	for index, rawTrack := range rawTracks {
		track, ok := rawTrack.(map[string]any)
		if !ok {
			return nil, 0, fmt.Errorf("malformed response: track %d is not an object", index+1)
		}
		normalized, err := normalizeHistoryTrack(track)
		if err != nil {
			return nil, 0, fmt.Errorf("malformed response: track %d: %w", index+1, err)
		}
		if normalized != nil {
			tracks = append(tracks, *normalized)
		}
	}
	return tracks, totalPages, nil
}

func normalizeHistoryTrack(track map[string]any) (*Scrobble, error) {
	attributes, _ := track["@attr"].(map[string]any)
	if nowPlaying, ok := attributes["nowplaying"].(string); ok && nowPlaying == "true" {
		// A now-playing track is not a historical scrobble, even when a
		// response includes a date for it.
		return nil, nil
	}

	rawDate, hasDate := track["date"]
	if !hasDate {
		return nil, errors.New("date is missing")
	}
	date, ok := rawDate.(map[string]any)
	if !ok {
		return nil, errors.New("date is not an object")
	}
	uts, ok := date["uts"].(string)
	if !ok || strings.TrimSpace(uts) == "" {
		return nil, errors.New("date.uts is missing")
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(uts), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("date.uts is invalid: %w", err)
	}

	artistObject, ok := track["artist"].(map[string]any)
	if !ok {
		return nil, errors.New("artist is missing")
	}
	artist, ok := artistObject["#text"].(string)
	if !ok || strings.TrimSpace(artist) == "" {
		return nil, errors.New("artist name is missing")
	}
	name, ok := track["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, errors.New("track name is missing")
	}
	return &Scrobble{
		Artist:    artist,
		Track:     name,
		Timestamp: time.Unix(seconds, 0).UTC(),
	}, nil
}

func historyInteger(parent map[string]any, objectName, fieldName string) (int, error) {
	rawObject, ok := parent[objectName].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("%s is missing", objectName)
	}
	rawValue, ok := rawObject[fieldName]
	if !ok {
		return 0, fmt.Errorf("%s is missing", fieldName)
	}
	switch value := rawValue.(type) {
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("%s is invalid: %w", fieldName, err)
		}
		return parsed, nil
	case float64:
		parsed := int(value)
		if value != float64(parsed) {
			return 0, fmt.Errorf("%s is not an integer", fieldName)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%s is invalid", fieldName)
	}
}
