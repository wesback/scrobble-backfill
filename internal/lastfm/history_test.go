package lastfm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type historyStatusRoundTripper struct {
	next       http.RoundTripper
	statusText string
}

func (t historyStatusRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err == nil && response.StatusCode >= http.StatusInternalServerError {
		response.Status = fmt.Sprintf("%d %s", response.StatusCode, t.statusText)
	}
	return response, err
}

func TestReadHistoryStreamsAllPagesAndSkipsNowPlaying(t *testing.T) {
	const sessionKey = "profile-session-secret"
	start := time.Unix(1788307200, 0).UTC()
	end := time.Unix(1788566340, 0).UTC()
	responses := map[string]string{
		"1": `{"recenttracks":{"track":[{"artist":{"#text":"Artist One"},"name":"Track One","date":{"uts":"1788307200"}},{"artist":{"#text":"Now"},"name":"Playing","@attr":{"nowplaying":"true"}}],"@attr":{"page":"1","totalPages":"3"}}}`,
		"2": `{"recenttracks":{"track":[{"artist":{"#text":"Now"},"name":"Playing With Date","date":{"uts":"1788393600"},"@attr":{"nowplaying":"true"}},{"artist":{"#text":"Artist Two"},"name":"Track Two","date":{"uts":"1788393600"}}],"@attr":{"page":"2","totalPages":"3"}}}`,
		"3": `{"recenttracks":{"track":[{"artist":{"#text":"Artist Three"},"name":"Track Three","date":{"uts":"1788566340"}}],"@attr":{"page":"3","totalPages":"3"}}}`,
	}
	var (
		mu       sync.Mutex
		requests []url.Values
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("request method = %s, want POST", r.Method)
		}
		if got := r.URL.Query().Get("sk"); got != "" {
			t.Errorf("session key leaked in URL query: %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, r.PostForm)
		mu.Unlock()
		fmt.Fprint(w, responses[r.PostForm.Get("page")])
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var got []Scrobble
	err := ReadHistory(context.Background(), client, AuthenticatedProfile{
		Username:   "alice",
		SessionKey: sessionKey,
	}, start, end, func(scrobble Scrobble) error {
		got = append(got, scrobble)
		return nil
	})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}

	want := []Scrobble{
		{Artist: "Artist One", Track: "Track One", Timestamp: time.Unix(1788307200, 0).UTC()},
		{Artist: "Artist Two", Track: "Track Two", Timestamp: time.Unix(1788393600, 0).UTC()},
		{Artist: "Artist Three", Track: "Track Three", Timestamp: time.Unix(1788566340, 0).UTC()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scrobbles = %#v, want %#v", got, want)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	for page, request := range requests {
		if request.Get("method") != "user.getRecentTracks" {
			t.Errorf("page %d method = %q", page+1, request.Get("method"))
		}
		if request.Get("user") != "alice" || request.Get("sk") != sessionKey {
			t.Errorf("page %d authenticated profile = user %q, session %q", page+1, request.Get("user"), request.Get("sk"))
		}
		if request.Get("from") != fmt.Sprint(start.Unix()) || request.Get("to") != fmt.Sprint(end.Unix()) {
			t.Errorf("page %d interval = [%s, %s]", page+1, request.Get("from"), request.Get("to"))
		}
		if request.Get("page") != fmt.Sprint(page+1) {
			t.Errorf("request page = %q, want %d", request.Get("page"), page+1)
		}
		wantSignature := signature(map[string]string{
			"api_key": "app-key",
			"from":    fmt.Sprint(start.Unix()),
			"method":  "user.getRecentTracks",
			"page":    fmt.Sprint(page + 1),
			"sk":      sessionKey,
			"to":      fmt.Sprint(end.Unix()),
			"user":    "alice",
		}, "app-secret")
		if request.Get("api_sig") != wantSignature {
			t.Errorf("page %d signature = %q, want %q", page+1, request.Get("api_sig"), wantSignature)
		}
	}
}

func TestReadHistoryReportsHTTPFailureWithoutSessionCredential(t *testing.T) {
	const sessionKey = "do-not-return-this-session"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "history unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL

	err := ReadHistory(context.Background(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: sessionKey,
	}, time.Unix(100, 0), time.Unix(200, 0), func(Scrobble) error { return nil })
	if err == nil {
		t.Fatal("expected history request error")
	}
	if !strings.Contains(err.Error(), "Last.fm history request") {
		t.Fatalf("error = %q, want history request context", err)
	}
	if strings.Contains(err.Error(), sessionKey) {
		t.Fatalf("error exposes session credential: %q", err)
	}
}

func TestReadHistoryReportsMalformedPayloadWithoutSessionCredential(t *testing.T) {
	const sessionKey = "malformed-payload-session"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"recenttracks":{"track":[{"artist":{"#text":"Artist"},"name":"Track","date":{"uts":"not-a-timestamp"}}],"@attr":{"totalPages":"1"}}}`)
	}))
	defer server.Close()
	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL

	err := ReadHistory(context.Background(), client, AuthenticatedProfile{
		Username: "alice", SessionKey: sessionKey,
	}, time.Unix(100, 0), time.Unix(200, 0), func(Scrobble) error { return nil })
	if err == nil {
		t.Fatal("expected malformed history error")
	}
	if !strings.Contains(err.Error(), "Last.fm history request") || !strings.Contains(err.Error(), "malformed response") {
		t.Fatalf("error = %q, want history request and malformed response context", err)
	}
	if strings.Contains(err.Error(), sessionKey) {
		t.Fatalf("error exposes session credential: %q", err)
	}
}

func TestHistoryReaderRetriesFailedPageWithoutRepeatingEarlierTracks(t *testing.T) {
	const sessionKey = "retry-session"
	var (
		mu     sync.Mutex
		pages  []string
		events []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		page := r.PostForm.Get("page")
		mu.Lock()
		pages = append(pages, page)
		events = append(events, "request "+page)
		mu.Unlock()
		switch page {
		case "1":
			fmt.Fprint(w, `{"recenttracks":{"track":[{"artist":{"#text":"First Artist"},"name":"First Track","date":{"uts":"100"}}],"@attr":{"totalPages":"2"}}}`)
		case "2":
			if len(pages) == 2 {
				http.Error(w, "temporary failure", http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, `{"recenttracks":{"track":[{"artist":{"#text":"Second Artist"},"name":"Second Track","date":{"uts":"200"}},{"artist":{"#text":"Third Artist"},"name":"Third Track","date":{"uts":"300"}}],"@attr":{"totalPages":"2"}}}`)
		default:
			t.Errorf("requested unexpected page %q", page)
		}
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var delays []time.Duration
	reader := NewHistoryReader(client, HistoryOptions{
		BaselineDelay:    5 * time.Millisecond,
		BaselineDelaySet: true,
		MaxRetries:       1,
		Sleep: func(_ context.Context, delay time.Duration) error {
			mu.Lock()
			delays = append(delays, delay)
			events = append(events, "sleep")
			mu.Unlock()
			return nil
		},
	})
	var got []Scrobble
	err := reader.Read(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: sessionKey,
	}, time.Unix(100, 0), time.Unix(300, 0), func(scrobble Scrobble) error {
		got = append(got, scrobble)
		return nil
	})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	want := []Scrobble{
		{Artist: "First Artist", Track: "First Track", Timestamp: time.Unix(100, 0).UTC()},
		{Artist: "Second Artist", Track: "Second Track", Timestamp: time.Unix(200, 0).UTC()},
		{Artist: "Third Artist", Track: "Third Track", Timestamp: time.Unix(300, 0).UTC()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scrobbles = %#v, want %#v", got, want)
	}
	mu.Lock()
	gotPages := append([]string(nil), pages...)
	gotEvents := append([]string(nil), events...)
	gotDelays := append([]time.Duration(nil), delays...)
	mu.Unlock()
	if !reflect.DeepEqual(gotPages, []string{"1", "2", "2"}) {
		t.Fatalf("requested pages = %v, want [1 2 2]", gotPages)
	}
	if !reflect.DeepEqual(gotDelays, []time.Duration{5 * time.Millisecond}) {
		t.Fatalf("retry delays = %v, want [%s]", gotDelays, 5*time.Millisecond)
	}
	if !reflect.DeepEqual(gotEvents, []string{"request 1", "request 2", "sleep", "request 2"}) {
		t.Fatalf("request/sleep order = %v, want [request 1 request 2 sleep request 2]", gotEvents)
	}
}

func TestHistoryReaderRetriesRateLimitedPage(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		pages = append(pages, r.PostForm.Get("page"))
		if len(pages) == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"recenttracks":{"track":[{"artist":{"#text":"Artist"},"name":"Track","date":{"uts":"100"}}],"@attr":{"totalPages":"1"}}}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var sleeps int
	reader := NewHistoryReader(client, HistoryOptions{
		BaselineDelay:    time.Millisecond,
		BaselineDelaySet: true,
		MaxRetries:       1,
		Sleep: func(context.Context, time.Duration) error {
			sleeps++
			return nil
		},
	})
	var got []Scrobble
	err := reader.Read(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: "rate-limit-session",
	}, time.Unix(100, 0), time.Unix(100, 0), func(scrobble Scrobble) error {
		got = append(got, scrobble)
		return nil
	})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if !reflect.DeepEqual(pages, []string{"1", "1"}) {
		t.Fatalf("requested pages = %v, want [1 1]", pages)
	}
	if sleeps != 1 {
		t.Fatalf("sleep calls = %d, want 1", sleeps)
	}
	if !reflect.DeepEqual(got, []Scrobble{{Artist: "Artist", Track: "Track", Timestamp: time.Unix(100, 0).UTC()}}) {
		t.Fatalf("scrobbles = %#v, want the one returned track", got)
	}
}

func TestHistoryReaderDoesNotRetryForbiddenPage(t *testing.T) {
	var requests, sleeps int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	reader := NewHistoryReader(client, HistoryOptions{
		BaselineDelay:    time.Millisecond,
		BaselineDelaySet: true,
		MaxRetries:       3,
		Sleep: func(context.Context, time.Duration) error {
			sleeps++
			return nil
		},
	})
	err := reader.Read(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: "forbidden-session",
	}, time.Unix(100, 0), time.Unix(200, 0), func(Scrobble) error { return nil })
	if err == nil {
		t.Fatal("expected history request error")
	}
	if err.Error() != "Last.fm history request page 1: Last.fm returned HTTP 403 Forbidden" {
		t.Fatalf("error = %q, want the original page 1 request error", err)
	}
	if requests != 1 || sleeps != 0 {
		t.Fatalf("requests = %d and sleeps = %d, want 1 request and no retries", requests, sleeps)
	}
}

func TestHistoryReaderReportsExhaustedRetriesForSamePage(t *testing.T) {
	const sessionKey = "retry-exhaustion-secret"
	var pages []string
	var delays []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		page := r.PostForm.Get("page")
		pages = append(pages, page)
		if page == "1" {
			fmt.Fprint(w, `{"recenttracks":{"track":[{"artist":{"#text":"First Artist"},"name":"First Track","date":{"uts":"100"}}],"@attr":{"totalPages":"2"}}}`)
			return
		}
		http.Error(w, "temporary failure", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	client.HTTPClient = &http.Client{Transport: historyStatusRoundTripper{
		next: http.DefaultTransport, statusText: sessionKey,
	}}
	reader := NewHistoryReader(client, HistoryOptions{
		BaselineDelay:    5 * time.Millisecond,
		BaselineDelaySet: true,
		MaxRetries:       2,
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	})
	var got []Scrobble
	err := reader.Read(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: sessionKey,
	}, time.Unix(100, 0), time.Unix(200, 0), func(scrobble Scrobble) error {
		got = append(got, scrobble)
		return nil
	})
	if err == nil {
		t.Fatal("expected exhausted history request error")
	}
	if err.Error() != "Last.fm history request page 2: Last.fm returned HTTP 500 [redacted]" {
		t.Fatalf("error = %q, want the redacted page 2 request error", err)
	}
	if strings.Contains(err.Error(), sessionKey) {
		t.Fatalf("error exposes session credential: %q", err)
	}
	if !reflect.DeepEqual(pages, []string{"1", "2", "2", "2"}) {
		t.Fatalf("requested pages = %v, want [1 2 2 2]", pages)
	}
	if !reflect.DeepEqual(delays, []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}) {
		t.Fatalf("retry delays = %v, want [5ms 10ms]", delays)
	}
	want := []Scrobble{{Artist: "First Artist", Track: "First Track", Timestamp: time.Unix(100, 0).UTC()}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scrobbles = %#v, want %#v", got, want)
	}
}

func TestHistoryReaderStopsWhenRetrySleepIsCanceled(t *testing.T) {
	var requests, sleeps int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "temporary failure", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := NewHistoryReader(client, HistoryOptions{
		BaselineDelay:    time.Millisecond,
		BaselineDelaySet: true,
		MaxRetries:       3,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			sleeps++
			cancel()
			return ctx.Err()
		},
	})
	err := reader.Read(ctx, AuthenticatedProfile{
		Username: "alice", SessionKey: "canceled-retry-session",
	}, time.Unix(100, 0), time.Unix(200, 0), func(Scrobble) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if requests != 1 || sleeps != 1 {
		t.Fatalf("requests = %d and sleeps = %d, want one request and one canceled wait", requests, sleeps)
	}
}
