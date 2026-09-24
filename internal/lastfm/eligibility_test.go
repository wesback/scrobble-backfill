package lastfm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wesback/scrobble-backfill/internal/spotify"
)

func TestEligibilityEvaluatorAppliesThresholdsAndFastPath(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []url.Values
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, r.PostForm)
		mu.Unlock()
		fmt.Fprint(w, `{"track":{"duration":"300000"}}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session-secret",
	})

	tests := []struct {
		name       string
		msPlayed   int64
		want       bool
		wantReason EligibilityReason
	}{
		{name: "under minimum", msPlayed: 29_999, wantReason: EligibilityReasonBelowMinimum},
		{name: "at minimum", msPlayed: 30_000, want: false, wantReason: EligibilityReasonBelowHalf},
		{name: "below half", msPlayed: 149_999, wantReason: EligibilityReasonBelowHalf},
		{name: "at half", msPlayed: 150_000, want: true, wantReason: EligibilityReasonAtLeastHalf},
		{name: "fast path", msPlayed: 240_000, want: true, wantReason: EligibilityReasonFastPath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := evaluator.Evaluate(context.Background(), spotify.Play{
				ArtistName: "Artist", TrackName: "Track", Milliseconds: test.msPlayed,
			})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if decision.Eligible != test.want {
				t.Fatalf("eligible = %t, want %t (%#v)", decision.Eligible, test.want, decision)
			}
			if decision.Reason != test.wantReason {
				t.Fatalf("reason = %q, want %q", decision.Reason, test.wantReason)
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("track.getInfo requests = %d, want 1", len(requests))
	}
	if requests[0].Get("method") != "track.getInfo" ||
		requests[0].Get("artist") != "Artist" ||
		requests[0].Get("track") != "Track" ||
		requests[0].Get("sk") != "session-secret" {
		t.Fatalf("request = %v, want authenticated track.getInfo request", requests[0])
	}
}

func TestEligibilityEvaluatorFastPathSkipsDurationLookup(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprint(w, `{"track":{"duration":"300000"}}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{
		Username: "alice", SessionKey: "session-secret",
	})

	decision, err := evaluator.Evaluate(context.Background(), spotify.Play{
		ArtistName: "Artist", TrackName: "Track", Milliseconds: 240_000,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !decision.Eligible {
		t.Fatalf("eligible = false, want true (%#v)", decision)
	}
	if decision.Reason != EligibilityReasonFastPath {
		t.Fatalf("reason = %q, want %q", decision.Reason, EligibilityReasonFastPath)
	}
	if requests != 0 {
		t.Fatalf("track.getInfo requests = %d, want 0", requests)
	}
}

func TestEligibilityEvaluatorCachesRepeatedNormalizedTrackLookups(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		fmt.Fprint(w, `{"track":{"duration":"200000"}}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{SessionKey: "session-secret"}, EligibilityOptions{
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	plays := []spotify.Play{
		{ArtistName: "Artist", TrackName: "Track", Milliseconds: 100_000},
		{ArtistName: " artist ", TrackName: "TRACK", Milliseconds: 99_999},
		{ArtistName: "Artist", TrackName: "Track", Milliseconds: 30_000},
	}
	want := []bool{true, false, false}
	for index, play := range plays {
		decision, err := evaluator.Evaluate(context.Background(), play)
		if err != nil {
			t.Fatalf("play %d evaluate: %v", index, err)
		}
		if decision.Eligible != want[index] {
			t.Errorf("play %d eligible = %t, want %t", index, decision.Eligible, want[index])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("track.getInfo requests = %d, want 1", requests)
	}
}

func TestEligibilityEvaluatorRetriesTransientDurationLookup(t *testing.T) {
	var (
		requests  int
		delays    []time.Duration
		callbacks int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, "temporary failure", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"track":{"duration":"180000"}}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{SessionKey: "session-secret"}, EligibilityOptions{
		BaselineDelay: 25 * time.Millisecond,
		MaxRetries:    2,
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
		LookupFailureHandler: func(string, string, error) { callbacks++ },
	})
	decision, err := evaluator.Evaluate(context.Background(), spotify.Play{
		ArtistName: "Artist", TrackName: "Track", Milliseconds: 100_000,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !decision.Eligible || decision.Reason != EligibilityReasonAtLeastHalf || decision.FallbackReason != "" {
		t.Fatalf("decision = %#v, want eligibility based on retried duration", decision)
	}
	if requests != 2 {
		t.Fatalf("track.getInfo requests = %d, want 2", requests)
	}
	if len(delays) != 1 || delays[0] != 25*time.Millisecond {
		t.Fatalf("retry delays = %v, want [25ms]", delays)
	}
	if callbacks != 0 {
		t.Fatalf("failure callbacks = %d, want 0", callbacks)
	}
}

func TestEligibilityEvaluatorDoesNotRetryNonRetryableDurationLookupFailure(t *testing.T) {
	var (
		requests  int
		delays    int
		callbacks int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":6,"message":"invalid session-secret"}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{SessionKey: "session-secret"}, EligibilityOptions{
		Sleep: func(context.Context, time.Duration) error {
			delays++
			return nil
		},
		LookupFailureHandler: func(artist, track string, err error) {
			callbacks++
			if artist != "Artist" || track != "Track" {
				t.Errorf("callback track = %q / %q, want Artist / Track", artist, track)
			}
			if err == nil || strings.Contains(err.Error(), "session-secret") {
				t.Errorf("callback error = %v, want session-redacted lookup error", err)
			}
		},
	})
	decision, err := evaluator.Evaluate(context.Background(), spotify.Play{
		ArtistName: "Artist", TrackName: "Track", Milliseconds: 100_000,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Eligible || decision.Reason != EligibilityReasonBelowHalf ||
		decision.FallbackReason != EligibilityFallbackLookupFailed {
		t.Fatalf("decision = %#v, want fail-closed lookup fallback", decision)
	}
	if requests != 1 {
		t.Fatalf("track.getInfo requests = %d, want 1", requests)
	}
	if delays != 0 {
		t.Fatalf("retry sleeps = %d, want 0", delays)
	}
	if callbacks != 1 {
		t.Fatalf("failure callbacks = %d, want 1", callbacks)
	}
}

func TestEligibilityEvaluatorCachesExhaustedDurationLookupFailure(t *testing.T) {
	var (
		requests  int
		delays    int
		callbacks int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{SessionKey: "session-secret"}, EligibilityOptions{
		BaselineDelay: time.Millisecond,
		MaxRetries:    2,
		Sleep: func(context.Context, time.Duration) error {
			delays++
			return nil
		},
		LookupFailureHandler: func(artist, track string, err error) {
			callbacks++
			if artist != "Artist" || track != "Track" {
				t.Errorf("callback track = %q / %q, want Artist / Track", artist, track)
			}
			if err == nil {
				t.Error("callback error = nil, want lookup failure")
			}
		},
	})
	play := spotify.Play{ArtistName: "Artist", TrackName: "Track", Milliseconds: 100_000}
	for attempt := 0; attempt < 2; attempt++ {
		decision, err := evaluator.Evaluate(context.Background(), play)
		if err != nil {
			t.Fatalf("evaluate %d: %v", attempt+1, err)
		}
		if decision.Eligible || decision.FallbackReason != EligibilityFallbackLookupFailed {
			t.Fatalf("decision %d = %#v, want fail-closed lookup fallback", attempt+1, decision)
		}
	}
	if requests != 3 {
		t.Fatalf("track.getInfo requests = %d, want 3 total attempts", requests)
	}
	if delays != 2 {
		t.Fatalf("retry sleeps = %d, want 2", delays)
	}
	if callbacks != 1 {
		t.Fatalf("failure callbacks = %d, want 1", callbacks)
	}
}

func TestEligibilityEvaluatorConservativelyFallsBackForUnavailableDurations(t *testing.T) {
	tests := []struct {
		name         string
		response     string
		status       int
		wantFallback EligibilityFallbackReason
	}{
		{name: "missing", response: `{"track":{}}`, wantFallback: EligibilityFallbackMissingDuration},
		{name: "zero", response: `{"track":{"duration":"0"}}`, wantFallback: EligibilityFallbackMissingDuration},
		{name: "malformed", response: `{"track":{"duration":"not-a-duration"}}`, wantFallback: EligibilityFallbackMalformedDuration},
		{name: "non-success", response: `{"error":6,"message":"session-secret should not escape"}`, status: http.StatusBadGateway, wantFallback: EligibilityFallbackLookupFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status != 0 {
					http.Error(w, test.response, test.status)
					return
				}
				fmt.Fprint(w, test.response)
			}))
			defer server.Close()

			client := NewClient("app-key", "app-secret")
			client.BaseURL = server.URL
			evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{SessionKey: "session-secret"}, EligibilityOptions{
				Sleep: func(context.Context, time.Duration) error { return nil },
			})
			decision, err := evaluator.Evaluate(context.Background(), spotify.Play{
				ArtistName: "Artist", TrackName: "Track", Milliseconds: 100_000,
			})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if decision.Eligible {
				t.Fatal("eligible = true, want false")
			}
			if decision.FallbackReason != test.wantFallback {
				t.Fatalf("fallback reason = %q, want %q", decision.FallbackReason, test.wantFallback)
			}
			if strings.Contains(fmt.Sprintf("%#v", decision), "session-secret") {
				t.Fatalf("decision exposes session credential: %#v", decision)
			}
		})
	}
}
