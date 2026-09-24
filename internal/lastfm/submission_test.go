package lastfm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wesback/scrobble-backfill/internal/journal"
)

const testSubmissionSession = "submission-session-secret"

func TestSubmissionBatchesAtFiftyAndResumesWithoutRedispatchingSubmittedBatch(t *testing.T) {
	store := journal.NewFileStore(filepath.Join(t.TempDir(), "journal"))
	run, err := store.CreateRun("personal", "import-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	plays := make([]journal.Submission, 51)
	for index := range plays {
		plays[index] = journal.Submission{
			Artist:    "Artist",
			Track:     fmt.Sprintf("Track %02d", index),
			Timestamp: time.Unix(int64(index+1), 0).UTC(),
		}
	}

	requests := make([][]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		if got := r.PostForm.Get("method"); got != "track.scrobble" {
			t.Errorf("method = %q, want track.scrobble", got)
		}
		if got := r.PostForm.Get("sk"); got != testSubmissionSession {
			t.Errorf("session = %q, want authenticated session", got)
		}
		signatureParams := make(map[string]string, len(r.PostForm))
		for key, values := range r.PostForm {
			signatureParams[key] = values[0]
		}
		if got, want := r.PostForm.Get("api_sig"), signature(signatureParams, "app-secret"); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
		tracks := make([]string, 0)
		for index := 0; ; index++ {
			track, ok := r.PostForm[fmt.Sprintf("track[%d]", index)]
			if !ok {
				break
			}
			tracks = append(tracks, track[0])
		}
		requests = append(requests, tracks)
		if len(requests) == 2 {
			http.Error(w, "interrupted", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	service := NewSubmissionService(client, store, SubmissionOptions{BaselineDelay: 0})
	profile := AuthenticatedProfile{Username: "alice", SessionKey: testSubmissionSession}
	if err := service.Submit(context.Background(), profile, run, plays); err == nil {
		t.Fatal("first submission unexpectedly succeeded")
	}

	resumeRun, err := store.OpenRun("personal", "import-1")
	if err != nil {
		t.Fatalf("open interrupted run: %v", err)
	}
	if err := service.Submit(context.Background(), profile, resumeRun, plays); err != nil {
		t.Fatalf("resume submission: %v", err)
	}

	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	if len(requests[0]) != 50 || len(requests[1]) != 1 || len(requests[2]) != 1 {
		t.Fatalf("request batch sizes = [%d %d %d], want [50 1 1]", len(requests[0]), len(requests[1]), len(requests[2]))
	}
	if requests[0][0] != "Track 00" || requests[0][49] != "Track 49" {
		t.Fatalf("first batch order = %q ... %q, want Track 00 ... Track 49", requests[0][0], requests[0][49])
	}
	if requests[1][0] != requests[2][0] || requests[1][0] != "Track 50" {
		t.Fatalf("resumed track requests = %q and %q, want Track 50", requests[1][0], requests[2][0])
	}
	final, err := store.Resume("personal", "import-1")
	if err != nil {
		t.Fatalf("resume final journal: %v", err)
	}
	if len(final.Submitted) != 2 || len(final.Pending) != 0 {
		t.Fatalf("final journal state = submitted %#v pending %#v, want two submitted batches", final.Submitted, final.Pending)
	}
}

func TestSubmissionReportsProgressAfterEachBatchIsMarkedSubmitted(t *testing.T) {
	store := journal.NewFileStore(filepath.Join(t.TempDir(), "journal"))
	run, err := store.CreateRun("personal", "import-progress")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	plays := make([]journal.Submission, 101)
	for index := range plays {
		plays[index] = journal.Submission{
			Artist:    "Artist",
			Track:     fmt.Sprintf("Track %03d", index),
			Timestamp: time.Unix(int64(index+1), 0),
		}
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		if method := r.PostForm.Get("method"); method != "track.scrobble" {
			t.Errorf("method = %q, want track.scrobble", method)
		}
		requests++
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	type progressUpdate struct {
		completed int
		total     int
	}
	var updates []progressUpdate
	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	service := NewSubmissionService(client, store, SubmissionOptions{
		BaselineDelay:    0,
		BaselineDelaySet: true,
		Progress: func(completed, total int) error {
			resume, err := store.Resume(run.Profile, run.InvocationID)
			if err != nil {
				return fmt.Errorf("read journal during progress callback: %w", err)
			}
			if len(resume.Submitted) != completed {
				return fmt.Errorf("journal has %d submitted batches at progress %d", len(resume.Submitted), completed)
			}
			updates = append(updates, progressUpdate{completed: completed, total: total})
			return nil
		},
	})
	if err := service.Submit(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: testSubmissionSession,
	}, run, plays); err != nil {
		t.Fatalf("submit: %v", err)
	}

	want := []progressUpdate{{1, 3}, {2, 3}, {3, 3}}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("progress updates = %#v, want %#v", updates, want)
	}
	if requests != 3 {
		t.Fatalf("submission requests = %d, want 3", requests)
	}
}

func TestSubmissionReportsOnlySuccessfulBatchesBeforeFailure(t *testing.T) {
	store := journal.NewFileStore(filepath.Join(t.TempDir(), "journal"))
	run, err := store.CreateRun("personal", "import-partial-progress")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	plays := make([]journal.Submission, 151)
	for index := range plays {
		plays[index] = journal.Submission{
			Artist:    "Artist",
			Track:     fmt.Sprintf("Track %03d", index),
			Timestamp: time.Unix(int64(index+1), 0),
		}
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		if method := r.PostForm.Get("method"); method != "track.scrobble" {
			t.Errorf("method = %q, want track.scrobble", method)
		}
		requests++
		if requests <= 2 {
			fmt.Fprint(w, `{}`)
			return
		}
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	type progressUpdate struct {
		completed int
		total     int
	}
	var updates []progressUpdate
	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	service := NewSubmissionService(client, store, SubmissionOptions{
		BaselineDelay: 0,
		MaxRetries:    1,
		Sleep:         func(context.Context, time.Duration) error { return nil },
		Progress: func(completed, total int) error {
			resume, err := store.Resume(run.Profile, run.InvocationID)
			if err != nil {
				return fmt.Errorf("read journal during progress callback: %w", err)
			}
			if len(resume.Submitted) != completed {
				return fmt.Errorf("journal has %d submitted batches at progress %d", len(resume.Submitted), completed)
			}
			updates = append(updates, progressUpdate{completed: completed, total: total})
			return nil
		},
	})
	err = service.Submit(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: testSubmissionSession,
	}, run, plays)
	if err == nil {
		t.Fatal("expected third batch submission to fail")
	}

	want := []progressUpdate{{1, 4}, {2, 4}}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("progress updates = %#v, want %#v", updates, want)
	}
	if requests != 4 {
		t.Fatalf("submission requests = %d, want two successful requests and two failed attempts", requests)
	}
	resume, err := store.Resume(run.Profile, run.InvocationID)
	if err != nil {
		t.Fatalf("resume partially submitted run: %v", err)
	}
	if len(resume.Submitted) != 2 || len(resume.Pending) != 1 || resume.Pending[0].State != journal.StatePlanned {
		t.Fatalf("journal after failed batch = %#v, want two submitted batches and one planned", resume)
	}
}

func TestSubmissionUsesExponentialRetryDelaysAndPacesBatches(t *testing.T) {
	store := journal.NewFileStore(filepath.Join(t.TempDir(), "journal"))
	run, err := store.CreateRun("personal", "import-2")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	plays := make([]journal.Submission, 51)
	for index := range plays {
		plays[index] = journal.Submission{
			Artist:    "Artist",
			Track:     fmt.Sprintf("Track %02d", index),
			Timestamp: time.Unix(int64(index+1), 0),
		}
	}
	statuses := []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusOK, http.StatusOK}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := statuses[calls]
		calls++
		if status != http.StatusOK {
			http.Error(w, "retry later", status)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	var delays []time.Duration
	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	service := NewSubmissionService(client, store, SubmissionOptions{
		BaselineDelay: 250 * time.Millisecond,
		MaxRetries:    3,
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	})
	if err := service.Submit(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: testSubmissionSession,
	}, run, plays); err != nil {
		t.Fatalf("submit with retries: %v", err)
	}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 250 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for index := range want {
		if delays[index] != want[index] {
			t.Fatalf("delay %d = %s, want %s", index, delays[index], want[index])
		}
	}
}

func TestSubmissionLeavesFailedBatchPlannedAndRedactsSession(t *testing.T) {
	store := journal.NewFileStore(filepath.Join(t.TempDir(), "journal"))
	run, err := store.CreateRun("personal", "import-3")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not allowed", http.StatusForbidden)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	service := NewSubmissionService(client, store, SubmissionOptions{BaselineDelay: 0, MaxRetries: 1})
	err = service.Submit(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: testSubmissionSession,
	}, run, []journal.Submission{{
		Artist: "Artist", Track: "Track", Timestamp: time.Unix(1, 0),
	}})
	if err == nil {
		t.Fatal("expected non-retryable submission error")
	}
	if strings.Contains(err.Error(), testSubmissionSession) {
		t.Fatalf("submission error exposes session: %q", err)
	}
	data, err := os.ReadFile(filepath.Join(store.Root, "706572736f6e616c.json"))
	if err != nil {
		t.Fatalf("read failed submission journal: %v", err)
	}
	if strings.Contains(string(data), testSubmissionSession) {
		t.Fatalf("submission journal exposes session: %s", data)
	}
	resume, err := store.Resume("personal", "import-3")
	if err != nil {
		t.Fatalf("resume failed batch: %v", err)
	}
	if len(resume.Submitted) != 0 || len(resume.Pending) != 1 || resume.Pending[0].State != journal.StatePlanned {
		t.Fatalf("failed batch journal state = %#v", resume)
	}
}

func TestSubmissionLeavesExhaustedRetryBatchPlanned(t *testing.T) {
	store := journal.NewFileStore(filepath.Join(t.TempDir(), "journal"))
	run, err := store.CreateRun("personal", "import-4")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	service := NewSubmissionService(client, store, SubmissionOptions{
		BaselineDelay: 10 * time.Millisecond,
		MaxRetries:    2,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	})
	err = service.Submit(context.Background(), AuthenticatedProfile{
		Username: "alice", SessionKey: testSubmissionSession,
	}, run, []journal.Submission{{
		Artist: "Artist", Track: "Track", Timestamp: time.Unix(1, 0),
	}})
	if err == nil {
		t.Fatal("expected exhausted retry error")
	}
	if calls != 3 {
		t.Fatalf("request count = %d, want initial request plus two retries", calls)
	}
	resume, err := store.Resume("personal", "import-4")
	if err != nil {
		t.Fatalf("resume exhausted batch: %v", err)
	}
	if len(resume.Submitted) != 0 || len(resume.Pending) != 1 || resume.Pending[0].State != journal.StatePlanned {
		t.Fatalf("exhausted batch journal state = %#v", resume)
	}
}

func TestSubmissionRejectsChangedWhitespaceInJournaledPayload(t *testing.T) {
	store := journal.NewFileStore(filepath.Join(t.TempDir(), "journal"))
	run, err := store.CreateRun("personal", "import-whitespace")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	original := []journal.Submission{{
		Artist:    "Artist",
		Track:     "Track",
		Album:     "Album",
		Timestamp: time.Unix(1, 0).UTC(),
	}}
	if _, err := store.PlanBatch(run.Profile, run.InvocationID, original); err != nil {
		t.Fatalf("plan batch: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("changed payload must not be dispatched")
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	service := NewSubmissionService(client, store, SubmissionOptions{BaselineDelay: 0})
	changed := []journal.Submission{{
		Artist:    " Artist",
		Track:     "Track ",
		Album:     " Album",
		Timestamp: original[0].Timestamp,
	}}
	if err := service.Submit(context.Background(), AuthenticatedProfile{
		Username:   "alice",
		SessionKey: testSubmissionSession,
	}, run, changed); err == nil {
		t.Fatal("expected changed journal payload to be rejected")
	}
}
