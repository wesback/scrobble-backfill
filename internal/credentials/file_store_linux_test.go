//go:build linux

package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLinuxFileStoreRoundTripAndPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rescrobble", "credentials")
	store := newTestLinuxFileStore(root, "machine-a")
	var credentialsStore Store = store
	session := "session-secret-123"

	if err := credentialsStore.Save("personal", session); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	got, err := credentialsStore.Load("personal")
	if err != nil {
		t.Fatalf("load credential: %v", err)
	}
	if got != session {
		t.Fatalf("loaded credential = %q, want %q", got, session)
	}
	if has, err := credentialsStore.Has("personal"); err != nil || !has {
		t.Fatalf("Has(personal) = %t, %v; want true, nil", has, err)
	}

	path := store.pathFor("personal")
	encrypted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read encrypted credential: %v", err)
	}
	if strings.Contains(string(encrypted), session) {
		t.Fatal("stored credential contains plaintext session")
	}
	for name, path := range map[string]string{
		"directory":        root,
		"parent directory": filepath.Dir(root),
		"credential":       path,
		"lock":             store.lockPathFor("personal"),
		"encryption key":   filepath.Join(root, linuxCredentialKeyFile),
		"key lock":         filepath.Join(root, linuxCredentialKeyLock),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		wantMode := os.FileMode(0o600)
		if strings.Contains(name, "directory") {
			wantMode = 0o700
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Errorf("%s permissions = %04o, want %04o", name, got, wantMode)
		}
	}

	oldFile, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential before replacement: %v", err)
	}
	replacement := "replacement-session-secret"
	if err := credentialsStore.Save("personal", replacement); err != nil {
		t.Fatalf("replace credential: %v", err)
	}
	newFile, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential after replacement: %v", err)
	}
	if os.SameFile(oldFile, newFile) {
		t.Fatal("credential update modified the old file instead of replacing it")
	}
	if got, err := credentialsStore.Load("personal"); err != nil || got != replacement {
		t.Fatalf("load replaced credential = %q, %v; want %q", got, err, replacement)
	}

	if err := credentialsStore.Delete("missing"); err != nil {
		t.Fatalf("delete absent credential: %v", err)
	}
	if _, err := credentialsStore.Load("missing"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("load absent credential error = %v, want ErrCredentialNotFound", err)
	}
	if has, err := credentialsStore.Has("missing"); err != nil || has {
		t.Fatalf("Has(missing) = %t, %v; want false, nil", has, err)
	}
	if err := credentialsStore.Delete("personal"); err != nil {
		t.Fatalf("delete credential: %v", err)
	}
}

func TestLinuxFileStoreRejectsTamperedCiphertext(t *testing.T) {
	store := newTestLinuxFileStore(filepath.Join(t.TempDir(), "credentials"), "machine-a")
	if err := store.Save("personal", "session-secret"); err != nil {
		t.Fatalf("save credential: %v", err)
	}

	path := store.pathFor("personal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read encrypted credential: %v", err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("tamper with encrypted credential: %v", err)
	}
	if _, err := store.Load("personal"); err == nil || errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("load tampered credential error = %v, want authentication failure", err)
	}
}

func TestLinuxFileStoreMachineIdentityChangeIsNotFound(t *testing.T) {
	root := filepath.Join(t.TempDir(), "credentials")
	machineID := "machine-a"
	store := newTestLinuxFileStore(root, "")
	store.machineIdentity = func() (string, error) { return machineID, nil }
	if err := store.Save("personal", "session-secret"); err != nil {
		t.Fatalf("save credential: %v", err)
	}

	machineID = "machine-b"
	if _, err := store.Load("personal"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("load credential after machine identity change error = %v, want ErrCredentialNotFound", err)
	}
}

func TestLinuxFileStoreCopiedCredentialCannotBeDecryptedWithoutKeyFile(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "source")
	source := newTestLinuxFileStore(sourceRoot, "machine-a")
	if err := source.Save("personal", "session-secret"); err != nil {
		t.Fatalf("save source credential: %v", err)
	}

	copiedRoot := filepath.Join(t.TempDir(), "copied")
	if err := os.Mkdir(copiedRoot, 0o700); err != nil {
		t.Fatalf("create copied store directory: %v", err)
	}
	credential, err := os.ReadFile(source.pathFor("personal"))
	if err != nil {
		t.Fatalf("read source credential: %v", err)
	}
	copied := newTestLinuxFileStore(copiedRoot, "machine-a")
	if err := os.WriteFile(copied.pathFor("personal"), credential, 0o600); err != nil {
		t.Fatalf("copy credential file: %v", err)
	}

	if _, err := copied.Load("personal"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("load copied credential without key file error = %v, want ErrCredentialNotFound", err)
	}
}

func TestLinuxFileStoreProfilesUseDistinctContainedPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "credentials")
	store := newTestLinuxFileStore(root, "machine-a")
	profiles := []string{"../personal", "personal", "/personal"}

	paths := make(map[string]bool)
	for index, profile := range profiles {
		path := store.pathFor(profile)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relative path for profile %q: %v", profile, err)
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			t.Errorf("profile %q escaped storage directory to %q", profile, path)
		}
		if paths[path] {
			t.Errorf("profile %q collided with another profile path %q", profile, path)
		}
		paths[path] = true
		if err := store.Save(profile, "session-"+string(rune('a'+index))); err != nil {
			t.Fatalf("save profile %q: %v", profile, err)
		}
	}
	for index, profile := range profiles {
		got, err := store.Load(profile)
		if err != nil {
			t.Fatalf("load profile %q: %v", profile, err)
		}
		if want := "session-" + string(rune('a'+index)); got != want {
			t.Errorf("profile %q credential = %q, want %q", profile, got, want)
		}
	}
}

func TestLinuxFileStoreConcurrentStoresKeepAtomicCredentials(t *testing.T) {
	root := filepath.Join(t.TempDir(), "credentials")
	first := newTestLinuxFileStore(root, "machine-a")
	second := newTestLinuxFileStore(root, "machine-a")

	var wait sync.WaitGroup
	errors := make(chan error, 20)
	for index := 0; index < 20; index++ {
		wait.Add(1)
		store := first
		if index%2 != 0 {
			store = second
		}
		session := "session-" + string(rune('a'+index))
		go func() {
			defer wait.Done()
			errors <- store.Save("personal", session)
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent save: %v", err)
		}
	}
	if got, err := first.Load("personal"); err != nil || !strings.HasPrefix(got, "session-") {
		t.Fatalf("load after concurrent saves = %q, %v", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read credential directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") || strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("temporary file left after atomic save: %q", entry.Name())
		}
	}
}

func TestLinuxFileStoreSaveWaitsForProfileLock(t *testing.T) {
	store := newTestLinuxFileStore(filepath.Join(t.TempDir(), "credentials"), "machine-a")
	if err := store.ensureRoot(); err != nil {
		t.Fatalf("create credential directory: %v", err)
	}
	lock, err := acquireLinuxCredentialLock(store.lockPathFor("personal"))
	if err != nil {
		t.Fatalf("acquire profile lock: %v", err)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- store.Save("personal", "session-secret")
	}()
	<-started
	select {
	case err := <-done:
		_ = lock.Close()
		t.Fatalf("save completed while the profile lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release profile lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("save after releasing profile lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("save did not complete after releasing profile lock")
	}
}

func TestLinuxDefaultCredentialPathIsUnderUserConfigDir(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	path, err := LinuxDefaultPath()
	if err != nil {
		t.Fatalf("get Linux default path: %v", err)
	}
	want := filepath.Join(configDir, "rescrobble", "credentials")
	if path != want {
		t.Fatalf("Linux default path = %q, want %q", path, want)
	}
}

func newTestLinuxFileStore(root, machineID string) *LinuxFileStore {
	store := NewLinuxFileStore(root)
	store.machineIdentity = func() (string, error) { return machineID, nil }
	store.owningUserID = func() (string, error) { return "1000", nil }
	return store
}
