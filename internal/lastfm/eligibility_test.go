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
	evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{SessionKey: "session-secret"})
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
			evaluator := NewEligibilityEvaluator(client, AuthenticatedProfile{SessionKey: "session-secret"})
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
