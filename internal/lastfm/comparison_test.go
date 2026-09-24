package lastfm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/wesback/scrobble-backfill/internal/spotify"
)

func TestCompareConvertsInclusiveLocalDatesAndUsesDefaultTolerance(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	playTime := time.Date(2024, 1, 1, 0, 0, 0, 0, location)
	var request url.Values
	server := comparisonServer(t, func(values url.Values) string {
		request = values
		return historyResponse("Artist", "Track", playTime.UTC())
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	summary, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:     time.Date(2024, 1, 1, 15, 0, 0, 0, time.UTC),
		To:       time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC),
		Timezone: location,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: playTime, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Outside", Timestamp: playTime.Add(24 * time.Hour), Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	wantStart := playTime.UTC()
	wantEnd := time.Date(2024, 1, 2, 0, 0, 0, 0, location).UTC().Add(-time.Nanosecond)
	if summary.FromUTC != wantStart || !summary.ToUTC.Equal(wantEnd) {
		t.Fatalf("UTC bounds = [%s, %s], want [%s, %s]", summary.FromUTC, summary.ToUTC, wantStart, wantEnd)
	}
	if summary.TimestampTolerance != DefaultTimestampTolerance {
		t.Fatalf("tolerance = %s, want default %s", summary.TimestampTolerance, DefaultTimestampTolerance)
	}
	if request.Get("from") != fmt.Sprint(wantStart.Unix()) || request.Get("to") != fmt.Sprint(wantEnd.Unix()) {
		t.Fatalf("history request bounds = [%s, %s], want [%d, %d]", request.Get("from"), request.Get("to"), wantStart.Unix(), wantEnd.Unix())
	}
	if len(results) != 1 || results[0].Status != ComparisonStatusMatched || results[0].Confidence != ComparisonConfidenceHigh {
		t.Fatalf("results = %#v, want one high-confidence match", results)
	}
}

func TestCompareUsesOverriddenToleranceAndConfidenceTiers(t *testing.T) {
	base := time.Date(2024, 2, 3, 12, 0, 0, 0, time.UTC)
	server := comparisonServer(t, func(url.Values) string {
		return `{"recenttracks":{"track":[` +
			historyTrack("Artist", "Medium", base.Add(-10*time.Second)) + "," +
			historyTrack("Artist", "Low", base.Add(-20*time.Second)) +
			`],"@attr":{"totalPages":"1"}}}`
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	summary, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:               base,
		To:                 base,
		Timezone:           time.UTC,
		TimestampTolerance: 20 * time.Second,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "Medium", Timestamp: base, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Low", Timestamp: base, Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if summary.TimestampTolerance != 20*time.Second {
		t.Fatalf("tolerance = %s, want 20s", summary.TimestampTolerance)
	}
	want := []ComparisonResult{
		{Status: ComparisonStatusMatched, Confidence: ComparisonConfidenceMedium},
		{Status: ComparisonStatusMissing, Confidence: ComparisonConfidenceLow},
	}
	for index := range want {
		if results[index].Status != want[index].Status || results[index].Confidence != want[index].Confidence {
			t.Errorf("result %d = status %q confidence %q, want %q %q", index, results[index].Status, results[index].Confidence, want[index].Status, want[index].Confidence)
		}
	}
	if summary.Matched != 1 || summary.Missing != 1 || summary.MediumConfidence != 1 || summary.LowConfidence != 1 {
		t.Fatalf("summary = %#v, want one medium match and one low missing result", summary)
	}
}

func TestCompareReadsHistoryBeforeSpotifySourceCompletes(t *testing.T) {
	base := time.Date(2024, 2, 3, 12, 0, 0, 0, time.UTC)
	historyCalls := 0
	server := comparisonServer(t, func(url.Values) string {
		historyCalls++
		return historyResponse("Artist", "Track", base)
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	plays := []spotify.Play{
		{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000},
		{ArtistName: "Artist", TrackName: "Track", Timestamp: base.Add(5 * time.Minute), Milliseconds: 240_000},
	}
	var results []ComparisonResult
	summary, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:     base,
		To:       base,
		Timezone: time.UTC,
		Plays: func(ctx context.Context, consume spotify.Consumer) error {
			for index, play := range plays {
				if err := consume(play); err != nil {
					return err
				}
				if index == 0 && historyCalls != 0 {
					t.Fatalf("history was read before a group became stale")
				}
				if index == 1 && historyCalls != 1 {
					t.Fatalf("history calls after stale group flush = %d, want 1", historyCalls)
				}
			}
			return nil
		},
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if summary.Eligible != 2 || len(results) != 2 || historyCalls != 1 {
		t.Fatalf("summary = %#v, results = %d, history calls = %d; want two results and one history read", summary, len(results), historyCalls)
	}
}

func TestCompareReadsEachHistoryPageOnceForDisjointTrackGroups(t *testing.T) {
	base := time.Date(2024, 2, 3, 12, 0, 0, 0, time.UTC)
	pageRequests := make(map[string]int)
	server := comparisonServer(t, func(values url.Values) string {
		page := values.Get("page")
		pageRequests[page]++
		switch page {
		case "1":
			return `{"recenttracks":{"track":[` +
				historyTrack("Artist", "First", base) +
				`],"@attr":{"totalPages":"2"}}}`
		case "2":
			return `{"recenttracks":{"track":[` +
				historyTrack("Artist", "Third", base.Add(10*time.Minute+3*time.Second)) +
				`],"@attr":{"totalPages":"2"}}}`
		default:
			t.Errorf("requested unexpected history page %q", page)
			return `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`
		}
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	summary, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:               base,
		To:                 base,
		Timezone:           time.UTC,
		TimestampTolerance: 10 * time.Second,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "First", Timestamp: base, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Second", Timestamp: base.Add(5 * time.Minute), Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Third", Timestamp: base.Add(10 * time.Minute), Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(results) != 3 ||
		results[0].Status != ComparisonStatusMatched || results[0].Confidence != ComparisonConfidenceHigh ||
		results[1].Status != ComparisonStatusMissing ||
		results[2].Status != ComparisonStatusMatched || results[2].Confidence != ComparisonConfidenceHigh {
		t.Fatalf("results = %#v, want matched, missing, matched with high confidence for matches", results)
	}
	if summary.Matched != 2 || summary.Missing != 1 || summary.LastFMScrobbles != 2 {
		t.Fatalf("summary = %#v, want two matches, one missing, and two history records", summary)
	}
	if pageRequests["1"] != 1 || pageRequests["2"] != 1 || len(pageRequests) != 2 {
		t.Fatalf("history page requests = %v, want each of pages 1 and 2 exactly once", pageRequests)
	}
}

func TestCompareMatchesEachRepeatedHistoryOccurrenceAtMostOnce(t *testing.T) {
	base := time.Date(2024, 2, 3, 12, 0, 0, 0, time.UTC)
	server := comparisonServer(t, func(url.Values) string {
		record := historyTrack("Artist", "Track", base)
		return `{"recenttracks":{"track":[` + record + "," + record + `],"@attr":{"totalPages":"1"}}}`
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	summary, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:     base,
		To:       base,
		Timezone: time.UTC,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(results) != 3 || summary.Matched != 2 || summary.Missing != 1 {
		t.Fatalf("summary = %#v, results = %#v; want two matches for two history occurrences", summary, results)
	}
	for index, want := range []ComparisonStatus{
		ComparisonStatusMatched,
		ComparisonStatusMatched,
		ComparisonStatusMissing,
	} {
		if results[index].Status != want {
			t.Errorf("result %d status = %q, want %q", index, results[index].Status, want)
		}
	}
}

func TestCompareIncludesDateBoundariesAndPreservesConfidenceTiers(t *testing.T) {
	start := time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1).Add(-time.Nanosecond)
	near := start.Add(10*time.Minute + 3*time.Second)
	variant := start.Add(20 * time.Minute)
	outside := end.Add(time.Second)
	server := comparisonServer(t, func(url.Values) string {
		return `{"recenttracks":{"track":[` +
			historyTrack("Artist", "Opening", start) + "," +
			historyTrack("Artist", "Near", near.Add(-3*time.Second)) + "," +
			historyTrack("Artist", "Tune", variant) + "," +
			historyTrack("Artist", "Closing", end) + "," +
			historyTrack("Artist", "Outside", outside) +
			`],"@attr":{"totalPages":"1"}}}`
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	summary, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:               start.Add(12 * time.Hour),
		To:                 start.Add(23 * time.Hour),
		Timezone:           time.UTC,
		TimestampTolerance: 20 * time.Second,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "Opening", Timestamp: start, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Near", Timestamp: near, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Tune (Remastered)", Timestamp: variant, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Outside", Timestamp: start.Add(30 * time.Minute), Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Closing", Timestamp: end, Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if summary.FromUTC != start || !summary.ToUTC.Equal(end) || summary.LastFMScrobbles != 4 {
		t.Fatalf("bounds = [%s, %s], history records = %d; want [%s, %s] and four in-range records",
			summary.FromUTC, summary.ToUTC, summary.LastFMScrobbles, start, end)
	}
	if len(results) != 5 {
		t.Fatalf("results = %d, want five", len(results))
	}
	for _, index := range []int{0, 1, 4} {
		if results[index].Status != ComparisonStatusMatched || results[index].Confidence != ComparisonConfidenceHigh {
			t.Errorf("result %d = status %q confidence %q, want high-confidence match", index, results[index].Status, results[index].Confidence)
		}
	}
	if results[2].Status != ComparisonStatusMissing ||
		results[2].Confidence != ComparisonConfidenceLow ||
		results[2].Scrobble == nil ||
		results[2].Scrobble.Track != "Tune" {
		t.Errorf("metadata variant result = %#v, want low-confidence missing with its candidate scrobble", results[2])
	}
	if results[3].Status != ComparisonStatusMissing || results[3].Scrobble != nil {
		t.Errorf("outside-range result = %#v, want missing without an out-of-range candidate", results[3])
	}
}

func TestCompareAllowsExplicitZeroTolerance(t *testing.T) {
	base := time.Date(2024, 2, 3, 12, 0, 0, 0, time.UTC)
	server := comparisonServer(t, func(url.Values) string {
		return historyResponse("Artist", "Track", base.Add(time.Second))
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var result ComparisonResult
	_, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:                  base,
		To:                    base,
		Timezone:              time.UTC,
		TimestampToleranceSet: true,
		Plays: sourceForTest(spotify.Play{
			ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000,
		}),
	}, func(got ComparisonResult) error {
		result = got
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if result.Status != ComparisonStatusMissing || result.Scrobble != nil {
		t.Fatalf("result = %#v, want exact-only missing result", result)
	}
}

func TestCompareClassifiesExactMatchAsHighConfidenceWithZeroTolerance(t *testing.T) {
	base := time.Date(2024, 2, 3, 12, 0, 0, 0, time.UTC)
	server := comparisonServer(t, func(url.Values) string {
		return historyResponse("Artist", "Track", base)
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var result ComparisonResult
	_, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:                  base,
		To:                    base,
		Timezone:              time.UTC,
		TimestampToleranceSet: true,
		Plays: sourceForTest(spotify.Play{
			ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000,
		}),
	}, func(got ComparisonResult) error {
		result = got
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if result.Status != ComparisonStatusMatched ||
		result.Confidence != ComparisonConfidenceHigh ||
		result.Scrobble == nil ||
		!result.Scrobble.Timestamp.Equal(base) {
		t.Fatalf("result = %#v, want exact high-confidence match", result)
	}
}

func TestCompareUsesMillisecondsToDisambiguateRepeatedPlays(t *testing.T) {
	base := time.Date(2024, 3, 4, 9, 0, 0, 0, time.UTC)
	server := comparisonServer(t, func(values url.Values) string {
		if values.Get("method") == "track.getInfo" {
			return `{"track":{"duration":"60000"}}`
		}
		return historyResponse("Artist", "Track", base)
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	_, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:     base,
		To:       base,
		Timezone: time.UTC,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 30_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Status != ComparisonStatusMissing || results[1].Status != ComparisonStatusMatched ||
		results[1].Confidence != ComparisonConfidenceMedium {
		t.Fatalf("repeated-play results = %#v, want short missing and medium-confidence full match", results)
	}
}

func TestCompareRematchesRepeatedPlaysWhenHistoryArrivesOutOfOrder(t *testing.T) {
	base := time.Date(2024, 3, 4, 9, 0, 0, 0, time.UTC)
	server := comparisonServer(t, func(url.Values) string {
		return `{"recenttracks":{"track":[` +
			historyTrack("Artist", "Track", base.Add(5*time.Second)) + "," +
			historyTrack("Artist", "Track", base) +
			`],"@attr":{"totalPages":"1"}}}`
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	_, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:               base,
		To:                 base,
		Timezone:           time.UTC,
		TimestampTolerance: 10 * time.Second,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base.Add(10 * time.Second), Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Status != ComparisonStatusMatched ||
		!results[0].Scrobble.Timestamp.Equal(base) ||
		results[1].Status != ComparisonStatusMatched ||
		!results[1].Scrobble.Timestamp.Equal(base.Add(5*time.Second)) {
		t.Fatalf("results = %#v, want rematched nearest assignments", results)
	}
}

func TestCompareDoesNotReuseHistoryAcrossOutOfOrderSourceGroups(t *testing.T) {
	base := time.Date(2024, 3, 4, 9, 0, 0, 0, time.UTC)
	historyCalls := 0
	server := comparisonServer(t, func(url.Values) string {
		historyCalls++
		return historyResponse("Artist", "Track", base)
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var results []ComparisonResult
	summary, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:     base,
		To:       base,
		Timezone: time.UTC,
		Plays: sourceForTest(
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base, Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base.Add(5 * time.Minute), Milliseconds: 240_000},
			spotify.Play{ArtistName: "Artist", TrackName: "Track", Timestamp: base.Add(5 * time.Second), Milliseconds: 240_000},
		),
	}, func(result ComparisonResult) error {
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(results) != 3 || summary.Matched != 1 || summary.Missing != 2 {
		t.Fatalf("summary = %#v, results = %d; want one match and two missing results", summary, len(results))
	}
	if summary.LastFMScrobbles != 1 {
		t.Fatalf("LastFMScrobbles = %d, want one unique history record", summary.LastFMScrobbles)
	}
	if historyCalls != 1 {
		t.Fatalf("history calls = %d, want one history read for all source groups", historyCalls)
	}
	if results[0].Status != ComparisonStatusMatched || results[1].Status != ComparisonStatusMissing ||
		results[2].Status != ComparisonStatusMissing {
		t.Fatalf("results = %#v, want only the first source play matched", results)
	}
}

func TestCompareTreatsMetadataVariantAsLowConfidenceMissing(t *testing.T) {
	base := time.Date(2024, 4, 5, 12, 0, 0, 0, time.UTC)
	server := comparisonServer(t, func(url.Values) string {
		return historyResponse("Artist", "Track", base)
	})
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var result ComparisonResult
	_, err := Compare(ctxForTest(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session",
	}, ComparisonRequest{
		From:     base,
		To:       base,
		Timezone: time.UTC,
		Plays: sourceForTest(spotify.Play{
			ArtistName: "Artist", TrackName: "Track (Remastered)", Timestamp: base, Milliseconds: 240_000,
		}),
	}, func(got ComparisonResult) error {
		result = got
		return nil
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if result.Status != ComparisonStatusMissing || result.Confidence != ComparisonConfidenceLow ||
		result.Scrobble == nil || result.Scrobble.Track != "Track" {
		t.Fatalf("result = %#v, want low-confidence missing variant", result)
	}
}

func TestScrobbleIdentityIsIndependentOfCallbackOrder(t *testing.T) {
	timestamp := time.Date(2024, 4, 5, 12, 0, 0, 0, time.UTC)
	records := []Scrobble{{
		Artist:    "Artist",
		Track:     "Track",
		Timestamp: timestamp,
	}, {
		Artist:    "artist",
		Track:     "Track",
		Timestamp: timestamp,
	}}
	firstOrder := map[string]string{
		records[0].Artist: scrobbleIdentity(records[0]),
		records[1].Artist: scrobbleIdentity(records[1]),
	}
	for index := len(records) - 1; index >= 0; index-- {
		if got := scrobbleIdentity(records[index]); got != firstOrder[records[index].Artist] {
			t.Fatalf("identity for %q changed when callback order changed", records[index].Artist)
		}
	}
	if firstOrder[records[0].Artist] == firstOrder[records[1].Artist] {
		t.Fatal("distinct history records must not share an identity")
	}
}

func comparisonServer(t *testing.T, response func(url.Values) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		fmt.Fprint(w, response(r.PostForm))
	}))
}

func historyResponse(artist, track string, timestamp time.Time) string {
	return `{"recenttracks":{"track":[` + historyTrack(artist, track, timestamp) + `],"@attr":{"totalPages":"1"}}}`
}

func historyTrack(artist, track string, timestamp time.Time) string {
	return fmt.Sprintf(`{"artist":{"#text":%q},"name":%q,"date":{"uts":%q}}`, artist, track, fmt.Sprint(timestamp.Unix()))
}

func sourceForTest(plays ...spotify.Play) PlaySource {
	return func(ctx context.Context, consumer spotify.Consumer) error {
		for _, play := range plays {
			if err := consumer(play); err != nil {
				return err
			}
		}
		return nil
	}
}

func ctxForTest() context.Context {
	return context.Background()
}
