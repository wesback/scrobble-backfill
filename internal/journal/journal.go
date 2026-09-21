// Package journal is the durable boundary for import intent and completion.
//
// It deliberately knows nothing about Last.fm credentials or network
// submission. Callers record a normalized batch as planned before dispatching
// it, then record success explicitly after the submission returns.
package journal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// JournalPathEnv allows tests and installations to select the journal
	// directory without changing the user's normal data location.
	JournalPathEnv = "RESCOBBLE_JOURNAL"

	// StatePlanned means the batch intent is durable and may be submitted.
	StatePlanned = "planned"
	// StateSubmitted means the caller recorded a successful submission.
	StateSubmitted = "submitted"
)

var (
	// ErrRunExists is returned when an invocation identity is reused.
	ErrRunExists = errors.New("journal run already exists")
	// ErrRunNotFound is returned when a profile or invocation has no run.
	ErrRunNotFound = errors.New("journal run not found")
	// ErrBatchNotFound is returned when a sequence is absent from a run.
	ErrBatchNotFound = errors.New("journal batch not found")
)

// Submission is the normalized payload for one scrobble submission.
//
// The shape is intentionally provider-facing metadata only. In particular,
// session credentials are not accepted or represented by this type.
type Submission struct {
	Artist    string    `json:"artist"`
	Track     string    `json:"track"`
	Album     string    `json:"album,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Duration  int       `json:"duration,omitempty"`
}

// Payload is an alternate domain name for Submission.
type Payload = Submission

// Batch is an ordered, durable unit of submission intent.
type Batch struct {
	Sequence    int          `json:"sequence"`
	Payloads    []Submission `json:"payloads"`
	State       string       `json:"state"`
	PlannedAt   time.Time    `json:"planned_at"`
	SubmittedAt *time.Time   `json:"submitted_at,omitempty"`
}

// Run is an immutable invocation identity and its append-only batch journal.
// Profile and InvocationID are never changed after CreateRun succeeds.
type Run struct {
	Profile      string    `json:"profile"`
	InvocationID string    `json:"invocation_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Batches      []Batch   `json:"batches"`
}

// Resume describes what a restarted invocation can do next. Submitted contains
// batches to exclude from dispatch; Resumable is the earliest planned batch.
type Resume struct {
	Run       Run     `json:"run"`
	Submitted []Batch `json:"submitted"`
	Pending   []Batch `json:"pending"`
	Resumable *Batch  `json:"resumable,omitempty"`
}

// Store is the durable journal boundary shared by import, resume, and
// reporting/diagnostics callers.
type Store interface {
	CreateRun(profile, invocationID string) (Run, error)
	OpenRun(profile, invocationID string) (Run, error)
	PlanBatch(profile, invocationID string, payloads []Submission) (Batch, error)
	PlanBatches(profile, invocationID string, payloads [][]Submission) ([]Batch, error)
	MarkSubmitted(profile, invocationID string, sequence int) error
	Resume(profile, invocationID string) (Resume, error)
	ListRuns(profile string) ([]Run, error)
}

// FileStore persists each profile's runs in a separate JSON file below Root.
// Files are named from a hex encoding of the profile, so valid profile names
// cannot escape Root or collide through path separators.
type FileStore struct {
	Root string
	mu   sync.Mutex
}

// NewFileStore creates a journal store rooted at path.
func NewFileStore(path string) *FileStore {
	return &FileStore{Root: path}
}

// NewStore is the concise constructor for a file-backed journal.
func NewStore(path string) *FileStore {
	return NewFileStore(path)
}

// DefaultPath returns the platform-specific journal directory, or the path
// selected by RESCOBBLE_JOURNAL when it is set.
func DefaultPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv(JournalPathEnv)); path != "" {
		return path, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("determine journal directory: %w", err)
	}
	return filepath.Join(configDir, "rescrobble", "journal"), nil
}

// NewDefaultStore creates a journal store at DefaultPath.
func NewDefaultStore() (*FileStore, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return NewFileStore(path), nil
}

// CreateRun durably creates a new invocation. Invocation identities are
// profile-scoped and immutable; creating an existing identity is rejected.
func (s *FileStore) CreateRun(profile, invocationID string) (Run, error) {
	if err := validateIdentity(profile, invocationID); err != nil {
		return Run{}, err
	}
	if err := s.validatePath(); err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireProfileLock(profile)
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = lock.Close() }()

	doc, err := s.load(profile)
	if err != nil {
		return Run{}, err
	}
	for _, run := range doc.Runs {
		if run.InvocationID == invocationID {
			return Run{}, fmt.Errorf("%w: %q for profile %q", ErrRunExists, invocationID, profile)
		}
	}
	now := time.Now().UTC()
	run := Run{
		Profile:      profile,
		InvocationID: invocationID,
		CreatedAt:    now,
		UpdatedAt:    now,
		Batches:      []Batch{},
	}
	doc.Runs = append(doc.Runs, run)
	if err := s.save(profile, doc); err != nil && !isCommittedSaveError(err) {
		return Run{}, err
	}
	return cloneRun(run), nil
}

// OpenRun reopens an existing invocation without changing it.
func (s *FileStore) OpenRun(profile, invocationID string) (Run, error) {
	if err := validateIdentity(profile, invocationID); err != nil {
		return Run{}, err
	}
	if err := s.validatePath(); err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireProfileLock(profile)
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = lock.Close() }()
	return s.findRun(profile, invocationID)
}

// PlanBatch records one normalized batch before a caller may dispatch it.
func (s *FileStore) PlanBatch(profile, invocationID string, payloads []Submission) (Batch, error) {
	batches, err := s.PlanBatches(profile, invocationID, [][]Submission{payloads})
	if err != nil {
		return Batch{}, err
	}
	return batches[0], nil
}

// PlanBatches atomically appends planned batches in the supplied order. Every
// batch is durable before this method returns, so dispatch can only begin
// after its intent has been recorded.
func (s *FileStore) PlanBatches(profile, invocationID string, payloads [][]Submission) ([]Batch, error) {
	if err := validateIdentity(profile, invocationID); err != nil {
		return nil, err
	}
	if len(payloads) == 0 {
		return nil, errors.New("journal batch list must not be empty")
	}
	if err := s.validatePath(); err != nil {
		return nil, err
	}
	normalized := make([][]Submission, len(payloads))
	for i, batch := range payloads {
		var err error
		normalized[i], err = normalizePayloads(batch)
		if err != nil {
			return nil, fmt.Errorf("batch %d: %w", i+1, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireProfileLock(profile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	doc, err := s.load(profile)
	if err != nil {
		return nil, err
	}
	runIndex := findRunIndex(doc.Runs, invocationID)
	if runIndex < 0 {
		return nil, fmt.Errorf("%w: %q for profile %q", ErrRunNotFound, invocationID, profile)
	}
	run := &doc.Runs[runIndex]
	nextSequence := len(run.Batches) + 1
	now := time.Now().UTC()
	added := make([]Batch, 0, len(normalized))
	for i, payload := range normalized {
		added = append(added, Batch{
			Sequence:  nextSequence + i,
			Payloads:  payload,
			State:     StatePlanned,
			PlannedAt: now,
		})
	}
	run.Batches = append(run.Batches, added...)
	run.UpdatedAt = now
	if err := s.save(profile, doc); err != nil && !isCommittedSaveError(err) {
		return nil, err
	}
	return cloneBatches(added), nil
}

// MarkSubmitted records a successful submission result. A failed write leaves
// the on-disk batch in its previous state because save is atomic.
func (s *FileStore) MarkSubmitted(profile, invocationID string, sequence int) error {
	if err := validateIdentity(profile, invocationID); err != nil {
		return err
	}
	if sequence < 1 {
		return errors.New("journal batch sequence must be positive")
	}
	if err := s.validatePath(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireProfileLock(profile)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	doc, err := s.load(profile)
	if err != nil {
		return err
	}
	runIndex := findRunIndex(doc.Runs, invocationID)
	if runIndex < 0 {
		return fmt.Errorf("%w: %q for profile %q", ErrRunNotFound, invocationID, profile)
	}
	run := &doc.Runs[runIndex]
	batchIndex := findBatchIndex(run.Batches, sequence)
	if batchIndex < 0 {
		return fmt.Errorf("%w: sequence %d in invocation %q", ErrBatchNotFound, sequence, invocationID)
	}
	batch := &run.Batches[batchIndex]
	if batch.State == StateSubmitted {
		return nil
	}
	if batch.State != StatePlanned {
		return fmt.Errorf("journal batch %d has invalid state %q", sequence, batch.State)
	}
	now := time.Now().UTC()
	batch.State = StateSubmitted
	batch.SubmittedAt = &now
	run.UpdatedAt = now
	err = s.save(profile, doc)
	if isCommittedSaveError(err) {
		return nil
	}
	return err
}

// Resume reopens a run and separates submitted batches from the remaining
// planned work. Resumable is the earliest planned batch, if one exists.
func (s *FileStore) Resume(profile, invocationID string) (Resume, error) {
	run, err := s.OpenRun(profile, invocationID)
	if err != nil {
		return Resume{}, err
	}
	result := Resume{Run: cloneRun(run)}
	for _, batch := range run.Batches {
		switch batch.State {
		case StateSubmitted:
			result.Submitted = append(result.Submitted, cloneBatch(batch))
		case StatePlanned:
			result.Pending = append(result.Pending, cloneBatch(batch))
			if result.Resumable == nil {
				next := cloneBatch(batch)
				result.Resumable = &next
			}
		default:
			return Resume{}, fmt.Errorf("journal batch %d has invalid state %q", batch.Sequence, batch.State)
		}
	}
	return result, nil
}

// ListRuns returns all retained invocations for a profile in creation order.
// Runs are never removed automatically.
func (s *FileStore) ListRuns(profile string) ([]Run, error) {
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	if err := s.validatePath(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireProfileLock(profile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	doc, err := s.load(profile)
	if err != nil {
		return nil, err
	}
	return cloneRuns(doc.Runs), nil
}

type document struct {
	Profile string `json:"profile"`
	Runs    []Run  `json:"runs"`
}

func (s *FileStore) validatePath() error {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return errors.New("journal path is empty")
	}
	return nil
}

func (s *FileStore) findRun(profile, invocationID string) (Run, error) {
	doc, err := s.load(profile)
	if err != nil {
		return Run{}, err
	}
	index := findRunIndex(doc.Runs, invocationID)
	if index < 0 {
		return Run{}, fmt.Errorf("%w: %q for profile %q", ErrRunNotFound, invocationID, profile)
	}
	return cloneRun(doc.Runs[index]), nil
}

func (s *FileStore) load(profile string) (document, error) {
	if err := s.validatePath(); err != nil {
		return document{}, err
	}
	data, err := os.ReadFile(s.pathFor(profile))
	if errors.Is(err, os.ErrNotExist) {
		return document{Profile: profile, Runs: []Run{}}, nil
	}
	if err != nil {
		return document{}, fmt.Errorf("read journal for profile %q: %w", profile, err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return document{}, fmt.Errorf("parse journal for profile %q: %w", profile, err)
	}
	if doc.Profile != profile {
		return document{}, fmt.Errorf("journal profile mismatch: file is for %q, requested %q", doc.Profile, profile)
	}
	if doc.Runs == nil {
		doc.Runs = []Run{}
	}
	for _, run := range doc.Runs {
		if run.Profile != profile {
			return document{}, fmt.Errorf("journal run profile mismatch for invocation %q", run.InvocationID)
		}
		if err := validateRun(run); err != nil {
			return document{}, err
		}
	}
	return doc, nil
}

func (s *FileStore) save(profile string, doc document) error {
	if err := s.validatePath(); err != nil {
		return err
	}
	if doc.Profile != profile {
		return errors.New("journal profile cannot be changed")
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode journal for profile %q: %w", profile, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("create journal directory %q: %w", s.Root, err)
	}
	temp, err := os.CreateTemp(s.Root, ".journal-*.tmp")
	if err != nil {
		return fmt.Errorf("create journal temporary file: %w", err)
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	defer cleanup()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set journal permissions: %w", err)
	}
	if _, err := io.Copy(temp, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync journal: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close journal: %w", err)
	}
	if err := os.Rename(tempName, s.pathFor(profile)); err != nil {
		return fmt.Errorf("replace journal for profile %q: %w", profile, err)
	}
	directory, err := os.Open(s.Root)
	if err != nil {
		return &committedSaveError{err: fmt.Errorf("open journal directory for sync: %w", err)}
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return &committedSaveError{err: fmt.Errorf("sync journal directory: %w", err)}
	}
	if err := directory.Close(); err != nil {
		return &committedSaveError{err: fmt.Errorf("close journal directory: %w", err)}
	}
	return nil
}

// committedSaveError reports a post-rename failure. The journal contents are
// already replaced, so callers must treat the operation as committed rather
// than reporting a failed state transition that could be retried ambiguously.
type committedSaveError struct {
	err error
}

func (e *committedSaveError) Error() string {
	return e.err.Error()
}

func (e *committedSaveError) Unwrap() error {
	return e.err
}

func isCommittedSaveError(err error) bool {
	var committed *committedSaveError
	return errors.As(err, &committed)
}

func (s *FileStore) pathFor(profile string) string {
	return filepath.Join(s.Root, hex.EncodeToString([]byte(profile))+".json")
}

func (s *FileStore) acquireProfileLock(profile string) (*profileLock, error) {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory %q: %w", s.Root, err)
	}
	lock, err := acquireProfileFileLock(filepath.Join(s.Root, hex.EncodeToString([]byte(profile))+".lock"))
	if err != nil {
		return nil, fmt.Errorf("lock journal for profile %q: %w", profile, err)
	}
	return lock, nil
}

func normalizePayloads(payloads []Submission) ([]Submission, error) {
	if len(payloads) == 0 {
		return nil, errors.New("journal batch payloads must not be empty")
	}
	normalized := make([]Submission, len(payloads))
	for i, payload := range payloads {
		payload.Artist = strings.TrimSpace(payload.Artist)
		payload.Track = strings.TrimSpace(payload.Track)
		payload.Album = strings.TrimSpace(payload.Album)
		payload.Timestamp = payload.Timestamp.UTC()
		if payload.Artist == "" {
			return nil, fmt.Errorf("payload %d artist must not be empty", i+1)
		}
		if payload.Track == "" {
			return nil, fmt.Errorf("payload %d track must not be empty", i+1)
		}
		if payload.Timestamp.IsZero() {
			return nil, fmt.Errorf("payload %d timestamp must not be zero", i+1)
		}
		if payload.Duration < 0 {
			return nil, fmt.Errorf("payload %d duration must not be negative", i+1)
		}
		normalized[i] = payload
	}
	return normalized, nil
}

func validateIdentity(profile, invocationID string) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	if strings.TrimSpace(invocationID) == "" {
		return errors.New("invocation identity must not be empty")
	}
	if invocationID != strings.TrimSpace(invocationID) {
		return errors.New("invocation identity must not start or end with whitespace")
	}
	return nil
}

func validateProfile(profile string) error {
	if strings.TrimSpace(profile) == "" {
		return errors.New("profile name must not be empty")
	}
	if profile != strings.TrimSpace(profile) {
		return errors.New("profile name must not start or end with whitespace")
	}
	return nil
}

func validateRun(run Run) error {
	if err := validateIdentity(run.Profile, run.InvocationID); err != nil {
		return err
	}
	for index, batch := range run.Batches {
		if batch.Sequence != index+1 {
			return fmt.Errorf("journal invocation %q has unordered batch sequence %d", run.InvocationID, batch.Sequence)
		}
		if batch.State != StatePlanned && batch.State != StateSubmitted {
			return fmt.Errorf("journal batch %d has invalid state %q", batch.Sequence, batch.State)
		}
		if len(batch.Payloads) == 0 {
			return fmt.Errorf("journal batch %d has no payloads", batch.Sequence)
		}
	}
	return nil
}

func findRunIndex(runs []Run, invocationID string) int {
	for index := range runs {
		if runs[index].InvocationID == invocationID {
			return index
		}
	}
	return -1
}

func findBatchIndex(batches []Batch, sequence int) int {
	for index := range batches {
		if batches[index].Sequence == sequence {
			return index
		}
	}
	return -1
}

func cloneRun(run Run) Run {
	run.Batches = cloneBatches(run.Batches)
	return run
}

func cloneRuns(runs []Run) []Run {
	cloned := make([]Run, len(runs))
	for i, run := range runs {
		cloned[i] = cloneRun(run)
	}
	return cloned
}

func cloneBatch(batch Batch) Batch {
	batch.Payloads = append([]Submission(nil), batch.Payloads...)
	if batch.SubmittedAt != nil {
		submittedAt := *batch.SubmittedAt
		batch.SubmittedAt = &submittedAt
	}
	return batch
}

func cloneBatches(batches []Batch) []Batch {
	cloned := make([]Batch, len(batches))
	for i, batch := range batches {
		cloned[i] = cloneBatch(batch)
	}
	return cloned
}
