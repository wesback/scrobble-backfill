package credentials

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

type fakeCredentialStore struct {
	sessions map[string]string
	err      error
}

func (f *fakeCredentialStore) Save(profile, session string) error {
	if f.err != nil {
		return f.err
	}
	if f.sessions == nil {
		f.sessions = make(map[string]string)
	}
	f.sessions[profile] = session
	return nil
}

func (f *fakeCredentialStore) Load(profile string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	session, ok := f.sessions[profile]
	if !ok {
		return "", ErrCredentialNotFound
	}
	return session, nil
}

func (f *fakeCredentialStore) Delete(profile string) error {
	if f.err != nil {
		return f.err
	}
	delete(f.sessions, profile)
	return nil
}

func (f *fakeCredentialStore) Has(profile string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	_, ok := f.sessions[profile]
	return ok, nil
}

func TestStoreInterfaceIsolatesProfilesAndDeletesOneCredential(t *testing.T) {
	var store Store = &fakeCredentialStore{}

	if err := store.Save("personal", "personal-session"); err != nil {
		t.Fatalf("save personal session: %v", err)
	}
	if err := store.Save("family", "family-session"); err != nil {
		t.Fatalf("save family session: %v", err)
	}

	for profile, want := range map[string]string{
		"personal": "personal-session",
		"family":   "family-session",
	} {
		got, err := store.Load(profile)
		if err != nil {
			t.Fatalf("load %s session: %v", profile, err)
		}
		if got != want {
			t.Fatalf("load %s session = %q, want %q", profile, got, want)
		}
		has, err := store.Has(profile)
		if err != nil {
			t.Fatalf("check %s session: %v", profile, err)
		}
		if !has {
			t.Fatalf("expected %s session to be present", profile)
		}
	}

	if err := store.Delete("personal"); err != nil {
		t.Fatalf("delete personal session: %v", err)
	}
	if has, err := store.Has("personal"); err != nil {
		t.Fatalf("check deleted personal session: %v", err)
	} else if has {
		t.Fatal("personal session still present after deletion")
	}
	if got, err := store.Load("family"); err != nil {
		t.Fatalf("load unaffected family session: %v", err)
	} else if got != "family-session" {
		t.Fatalf("family session after personal deletion = %q, want family-session", got)
	}
}

func TestStoreInterfacePropagatesUnavailableStoreError(t *testing.T) {
	wantErr := &UnavailableError{Cause: errors.New("Secret Service is unavailable")}
	var store Store = &fakeCredentialStore{err: wantErr}

	if err := store.Save("personal", "session"); !errors.Is(err, wantErr) {
		t.Fatalf("save error = %v, want %v", err, wantErr)
	}
	if _, err := store.Load("personal"); !errors.Is(err, wantErr) {
		t.Fatalf("load error = %v, want %v", err, wantErr)
	}
	if err := store.Delete("personal"); !errors.Is(err, wantErr) {
		t.Fatalf("delete error = %v, want %v", err, wantErr)
	}
	if _, err := store.Has("personal"); !errors.Is(err, wantErr) {
		t.Fatalf("has error = %v, want %v", err, wantErr)
	}
	if !strings.Contains(wantErr.Error(), "make an OS-native credential service available") {
		t.Fatalf("unavailable error = %q, want actionable setup guidance", wantErr)
	}
}

type fakeKeyring struct {
	sessions map[string]string
	err      error
}

func (f *fakeKeyring) Set(_ string, profile, session string) error {
	if f.err != nil {
		return f.err
	}
	if f.sessions == nil {
		f.sessions = make(map[string]string)
	}
	f.sessions[profile] = session
	return nil
}

func (f *fakeKeyring) Get(_ string, profile string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	session, ok := f.sessions[profile]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return session, nil
}

func (f *fakeKeyring) Delete(_ string, profile string) error {
	if f.err != nil {
		return f.err
	}
	delete(f.sessions, profile)
	return nil
}

func TestOSStoreUsesProfileAsCredentialKey(t *testing.T) {
	backend := &fakeKeyring{}
	store := &OSStore{backend: backend}

	if err := store.Save("personal", "personal-session"); err != nil {
		t.Fatalf("save personal session: %v", err)
	}
	if err := store.Save("family", "family-session"); err != nil {
		t.Fatalf("save family session: %v", err)
	}
	if err := store.Delete("personal"); err != nil {
		t.Fatalf("delete personal session: %v", err)
	}
	if got, err := store.Load("family"); err != nil {
		t.Fatalf("load family session: %v", err)
	} else if got != "family-session" {
		t.Fatalf("family session = %q, want family-session", got)
	}
	if _, err := store.Load("personal"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("personal load error = %v, want ErrCredentialNotFound", err)
	}
}

func TestOSStoreWrapsUnavailableBackend(t *testing.T) {
	backendErr := errors.New("Secret Service is unavailable")
	store := &OSStore{backend: &fakeKeyring{err: backendErr}}

	operations := []struct {
		name string
		run  func() error
	}{
		{name: "save", run: func() error { return store.Save("personal", "session") }},
		{name: "load", run: func() error {
			_, err := store.Load("personal")
			return err
		}},
		{name: "delete", run: func() error { return store.Delete("personal") }},
		{name: "has", run: func() error {
			_, err := store.Has("personal")
			return err
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			if !errors.Is(err, backendErr) {
				t.Fatalf("error = %v, want backend cause", err)
			}
			if !errors.Is(err, ErrSecureStoreUnavailable) {
				t.Fatalf("error = %v, want ErrSecureStoreUnavailable", err)
			}
			if !strings.Contains(err.Error(), "make an OS-native credential service available") {
				t.Fatalf("error = %q, want actionable setup guidance", err)
			}
		})
	}
}
