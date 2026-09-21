package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSubmission(track string) Submission {
	return Submission{
		Artist:    " Artist ",
		Track:     " " + track + " ",
		Album:     " Album ",
		Timestamp: time.Date(2026, 9, 21, 12, 30, 0, 0, time.FixedZone("test", 2*60*60)),
		Duration:  241,
	}
}

func TestJournalPersistsRunsAndResumesAfterAnInterruptedBatch(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(filepath.Join(root, "journal"))
	if _, err := store.CreateRun("personal", "invocation-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.PlanBatches("personal", "invocation-1", [][]Submission{
		{testSubmission("first")},
		{testSubmission("second")},
	}); err != nil {
		t.Fatalf("plan batches: %v", err)
	}
	if err := store.MarkSubmitted("personal", "invocation-1", 1); err != nil {
		t.Fatalf("mark first batch submitted: %v", err)
	}

	// A new store models a process restart. Only the successful first batch is
	// excluded; the interrupted second batch remains resumable.
	restarted := NewFileStore(filepath.Join(root, "journal"))
	resume, err := restarted.Resume("personal", "invocation-1")
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if len(resume.Submitted) != 1 || resume.Submitted[0].Sequence != 1 {
		t.Fatalf("submitted batches = %#v, want only sequence 1", resume.Submitted)
	}
	if len(resume.Pending) != 1 || resume.Pending[0].Sequence != 2 {
		t.Fatalf("pending batches = %#v, want only sequence 2", resume.Pending)
	}
	if resume.Resumable == nil || resume.Resumable.Sequence != 2 {
		t.Fatalf("resumable batch = %#v, want sequence 2", resume.Resumable)
	}
	if got := resume.Pending[0].Payloads[0].Track; got != "second" {
		t.Fatalf("resumable payload track = %q, want second", got)
	}
}

func TestJournalNormalizesPayloadsAndStoresNoSessionCredential(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(filepath.Join(root, "journal"))
	if _, err := store.CreateRun("personal", "invocation-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.PlanBatch("personal", "invocation-1", []Submission{testSubmission("track")}); err != nil {
		t.Fatalf("plan batch: %v", err)
	}

	run, err := NewFileStore(filepath.Join(root, "journal")).OpenRun("personal", "invocation-1")
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	payload := run.Batches[0].Payloads[0]
	if payload.Artist != "Artist" || payload.Track != "track" || payload.Album != "Album" {
		t.Fatalf("payload was not normalized: %#v", payload)
	}
	if payload.Timestamp.Location() != time.UTC {
		t.Fatalf("payload timestamp location = %v, want UTC", payload.Timestamp.Location())
	}

	data, err := os.ReadFile(filepath.Join(root, "journal", "706572736f6e616c.json"))
	if err != nil {
		t.Fatalf("read profile journal: %v", err)
	}
	if strings.Contains(string(data), "session-secret") || strings.Contains(string(data), "credential") {
		t.Fatalf("journal contains credential-shaped data: %s", data)
	}
}

func TestJournalIsolatesProfilesAndRetainsAllRuns(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(filepath.Join(root, "journal"))
	for _, profile := range []string{"personal", "family"} {
		if _, err := store.CreateRun(profile, "same-invocation"); err != nil {
			t.Fatalf("create %s run: %v", profile, err)
		}
		if _, err := store.PlanBatch(profile, "same-invocation", []Submission{testSubmission(profile)}); err != nil {
			t.Fatalf("plan %s batch: %v", profile, err)
		}
	}

	if err := store.MarkSubmitted("family", "same-invocation", 1); err != nil {
		t.Fatalf("mark family submitted: %v", err)
	}
	if _, err := store.OpenRun("personal", "unknown-invocation"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("opening unknown personal run error = %v, want ErrRunNotFound", err)
	}
	if _, err := store.OpenRun("family", "same-invocation"); err != nil {
		t.Fatalf("open family run: %v", err)
	}
	personal, err := NewFileStore(filepath.Join(root, "journal")).Resume("personal", "same-invocation")
	if err != nil {
		t.Fatalf("resume personal run: %v", err)
	}
	if len(personal.Submitted) != 0 || len(personal.Pending) != 1 {
		t.Fatalf("personal state = submitted %#v pending %#v, want planned only", personal.Submitted, personal.Pending)
	}
	if got := personal.Pending[0].Payloads[0].Track; got != "personal" {
		t.Fatalf("personal payload track = %q, want personal", got)
	}
	family, err := NewFileStore(filepath.Join(root, "journal")).Resume("family", "same-invocation")
	if err != nil {
		t.Fatalf("resume family run: %v", err)
	}
	if len(family.Submitted) != 1 || len(family.Pending) != 0 {
		t.Fatalf("family state = submitted %#v pending %#v, want submitted only", family.Submitted, family.Pending)
	}

	runs, err := store.ListRuns("personal")
	if err != nil {
		t.Fatalf("list personal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("personal run count = %d, want 1 retained run", len(runs))
	}
	if _, err := store.ListRuns("family"); err != nil {
		t.Fatalf("list family runs: %v", err)
	}
}

func TestJournalOnlyMarksPlannedBatchAfterSuccessfulStateTransition(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(filepath.Join(root, "journal"))
	if _, err := store.CreateRun("personal", "invocation-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.PlanBatch("personal", "invocation-1", []Submission{testSubmission("track")}); err != nil {
		t.Fatalf("plan batch: %v", err)
	}
	if err := store.MarkSubmitted("personal", "invocation-1", 2); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("failed transition error = %v, want ErrBatchNotFound", err)
	}
	resume, err := NewFileStore(filepath.Join(root, "journal")).Resume("personal", "invocation-1")
	if err != nil {
		t.Fatalf("resume after failed transition: %v", err)
	}
	if len(resume.Submitted) != 0 || resume.Resumable == nil || resume.Resumable.State != StatePlanned {
		t.Fatalf("state after failed transition = %#v, want planned batch only", resume)
	}
}

func TestJournalRejectsInvocationIdentityReuse(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "journal"))
	if _, err := store.CreateRun("personal", "invocation-1"); err != nil {
		t.Fatalf("create first run: %v", err)
	}
	if _, err := store.CreateRun("personal", "invocation-1"); !errors.Is(err, ErrRunExists) {
		t.Fatalf("duplicate invocation error = %v, want ErrRunExists", err)
	}
}

func TestJournalSerializesSeparateStoreInstances(t *testing.T) {
	root := filepath.Join(t.TempDir(), "journal")
	first := NewFileStore(root)
	second := NewFileStore(root)
	if _, err := first.CreateRun("personal", "invocation-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}

	const batchCount = 20
	errs := make(chan error, batchCount)
	for i := 0; i < batchCount; i++ {
		store := first
		if i%2 == 1 {
			store = second
		}
		go func(i int) {
			_, err := store.PlanBatch("personal", "invocation-1", []Submission{
				testSubmission("track-" + string(rune('a'+i))),
			})
			errs <- err
		}(i)
	}
	for i := 0; i < batchCount; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("plan concurrent batch: %v", err)
		}
	}

	run, err := NewFileStore(root).OpenRun("personal", "invocation-1")
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	if len(run.Batches) != batchCount {
		t.Fatalf("batch count = %d, want %d", len(run.Batches), batchCount)
	}
	for i, batch := range run.Batches {
		if batch.Sequence != i+1 {
			t.Fatalf("batch %d has sequence %d", i, batch.Sequence)
		}
	}
}
