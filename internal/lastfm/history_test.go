package lastfm

import (
	"context"
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
