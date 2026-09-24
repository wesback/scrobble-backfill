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

type policyStore struct {
	sessions  map[string]string
	saveErr   error
	loadErr   error
	deleteErr error
	hasErr    error
	calls     map[string]int
}

func (s *policyStore) record(operation string) {
	if s.calls == nil {
		s.calls = make(map[string]int)
	}
	s.calls[operation]++
}

func (s *policyStore) Save(profile, session string) error {
	s.record("save")
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.sessions == nil {
		s.sessions = make(map[string]string)
	}
	s.sessions[profile] = session
	return nil
}

func (s *policyStore) Load(profile string) (string, error) {
	s.record("load")
	if s.loadErr != nil {
		return "", s.loadErr
	}
	session, ok := s.sessions[profile]
	if !ok {
		return "", ErrCredentialNotFound
	}
	return session, nil
}

func (s *policyStore) Delete(profile string) error {
	s.record("delete")
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.sessions, profile)
	return nil
}

func (s *policyStore) Has(profile string) (bool, error) {
	s.record("has")
	if s.hasErr != nil {
		return false, s.hasErr
	}
	_, ok := s.sessions[profile]
	return ok, nil
}

func TestLinuxLoadWithTierDoesNotReportMissingFallbackAsSelected(t *testing.T) {
	native := &policyStore{loadErr: ErrSecureStoreUnavailable}
	store := &linuxFallbackStore{native: native}

	session, tier, nativeErr, err := store.LoadWithTier("personal")
	if session != "" {
		t.Fatalf("session = %q, want empty", session)
	}
	if tier != "" {
		t.Fatalf("tier = %q, want undetermined tier", tier)
	}
	if !errors.Is(nativeErr, ErrSecureStoreUnavailable) {
		t.Fatalf("native store error = %v, want ErrSecureStoreUnavailable", nativeErr)
	}
	if !errors.Is(err, ErrSecureStoreUnavailable) {
		t.Fatalf("load error = %v, want ErrSecureStoreUnavailable", err)
	}
	if got := native.calls["load"]; got != 1 {
		t.Fatalf("native Load calls = %d, want 1", got)
	}
}

func TestLinuxFallbackConsultedOnlyWhenNativeUnavailable(t *testing.T) {
	t.Run("save", func(t *testing.T) {
		otherErr := errors.New("write failed")
		tests := []struct {
			name          string
			nativeErr     error
			wantErr       error
			wantFallback  int
			wantNative    int
			wantFileValue string
		}{
			{name: "native unavailable", nativeErr: ErrSecureStoreUnavailable, wantFallback: 1, wantNative: 1, wantFileValue: "new-session"},
			{name: "native succeeds", wantNative: 1},
			{name: "not found is not unavailable", nativeErr: ErrCredentialNotFound, wantErr: ErrCredentialNotFound, wantNative: 1},
			{name: "other error", nativeErr: otherErr, wantErr: otherErr, wantNative: 1},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				native := &policyStore{saveErr: test.nativeErr}
				file := &policyStore{}
				store := NewPlatformStore("linux", native, file)
				err := store.Save("personal", "new-session")
				if test.wantErr != nil && !errors.Is(err, test.wantErr) {
					t.Fatalf("Save error = %v, want %v", err, test.wantErr)
				}
				if test.wantErr == nil && err != nil {
					t.Fatalf("Save error = %v, want nil", err)
				}
				if got := native.calls["save"]; got != test.wantNative {
					t.Errorf("native Save calls = %d, want %d", got, test.wantNative)
				}
				if got := file.calls["save"]; got != test.wantFallback {
					t.Errorf("fallback Save calls = %d, want %d", got, test.wantFallback)
				}
				if got := file.sessions["personal"]; got != test.wantFileValue {
					t.Errorf("fallback credential = %q, want %q", got, test.wantFileValue)
				}
			})
		}
	})

	t.Run("load", func(t *testing.T) {
		otherErr := errors.New("read failed")
		tests := []struct {
			name         string
			nativeErr    error
			nativeValue  string
			wantErr      error
			wantValue    string
			wantFallback int
		}{
			{name: "native unavailable", nativeErr: ErrSecureStoreUnavailable, wantFallback: 1, wantValue: "file-session"},
			{name: "native succeeds", nativeValue: "native-session", wantValue: "native-session"},
			{name: "credential not found", nativeErr: ErrCredentialNotFound, wantErr: ErrCredentialNotFound},
			{name: "other error", nativeErr: otherErr, wantErr: otherErr},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				native := &policyStore{loadErr: test.nativeErr, sessions: map[string]string{"personal": test.nativeValue}}
				file := &policyStore{sessions: map[string]string{"personal": "file-session"}}
				store := NewPlatformStore("linux", native, file)
				got, err := store.Load("personal")
				if test.wantErr != nil && !errors.Is(err, test.wantErr) {
					t.Fatalf("Load error = %v, want %v", err, test.wantErr)
				}
				if test.wantErr == nil && err != nil {
					t.Fatalf("Load error = %v, want nil", err)
				}
				if got != test.wantValue {
					t.Errorf("Load value = %q, want %q", got, test.wantValue)
				}
				if native.calls["load"] != 1 || file.calls["load"] != test.wantFallback {
					t.Errorf("Load calls = native %d, fallback %d; want 1, %d", native.calls["load"], file.calls["load"], test.wantFallback)
				}
			})
		}
	})

	t.Run("has", func(t *testing.T) {
		otherErr := errors.New("check failed")
		tests := []struct {
			name         string
			nativeErr    error
			nativeValue  bool
			wantErr      error
			wantValue    bool
			wantFallback int
		}{
			{name: "native unavailable", nativeErr: ErrSecureStoreUnavailable, wantFallback: 1, wantValue: true},
			{name: "native returns false", wantValue: false},
			{name: "native returns true", nativeValue: true, wantValue: true},
			{name: "credential not found", nativeErr: ErrCredentialNotFound, wantErr: ErrCredentialNotFound},
			{name: "other error", nativeErr: otherErr, wantErr: otherErr},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				native := &policyStore{hasErr: test.nativeErr}
				if test.nativeValue {
					native.sessions = map[string]string{"personal": "native-session"}
				}
				file := &policyStore{sessions: map[string]string{"personal": "file-session"}}
				store := NewPlatformStore("linux", native, file)
				got, err := store.Has("personal")
				if test.wantErr != nil && !errors.Is(err, test.wantErr) {
					t.Fatalf("Has error = %v, want %v", err, test.wantErr)
				}
				if test.wantErr == nil && err != nil {
					t.Fatalf("Has error = %v, want nil", err)
				}
				if got != test.wantValue {
					t.Errorf("Has value = %t, want %t", got, test.wantValue)
				}
				if native.calls["has"] != 1 || file.calls["has"] != test.wantFallback {
					t.Errorf("Has calls = native %d, fallback %d; want 1, %d", native.calls["has"], file.calls["has"], test.wantFallback)
				}
			})
		}
	})
}

func TestLinuxLogoutDeletesBothStoresAndPreservesOtherProfiles(t *testing.T) {
	native := &policyStore{sessions: map[string]string{
		"personal": "native-personal",
		"family":   "native-family",
	}}
	file := &policyStore{sessions: map[string]string{
		"personal": "dormant-personal",
		"family":   "dormant-family",
	}}
	store := NewPlatformStore("linux", native, file)

	if err := store.Delete("personal"); err != nil {
		t.Fatalf("delete personal credential: %v", err)
	}
	for tier, backend := range map[string]*policyStore{"native": native, "fallback": file} {
		if _, ok := backend.sessions["personal"]; ok {
			t.Errorf("%s store retained personal credential", tier)
		}
		if got := backend.sessions["family"]; got == "" {
			t.Errorf("%s store deleted the other profile's credential", tier)
		}
		if backend.calls["delete"] != 1 {
			t.Errorf("%s Delete calls = %d, want 1", tier, backend.calls["delete"])
		}
	}
}

func TestLinuxDeleteReportsNativeUnavailableAfterDeletingFallback(t *testing.T) {
	native := &policyStore{
		sessions:  map[string]string{"personal": "native-personal", "family": "native-family"},
		deleteErr: ErrSecureStoreUnavailable,
	}
	file := &policyStore{sessions: map[string]string{
		"personal": "dormant-personal",
		"family":   "dormant-family",
	}}
	store := NewPlatformStore("linux", native, file)

	if err := store.Delete("personal"); !errors.Is(err, ErrSecureStoreUnavailable) {
		t.Fatalf("delete error = %v, want ErrSecureStoreUnavailable", err)
	}
	if _, ok := native.sessions["personal"]; !ok {
		t.Fatal("unavailable native store unexpectedly changed its credential")
	}
	if _, ok := file.sessions["personal"]; ok {
		t.Fatal("fallback store retained personal credential")
	}
	if file.sessions["family"] != "dormant-family" {
		t.Fatalf("fallback family credential = %q, want it unchanged", file.sessions["family"])
	}
	if native.calls["delete"] != 1 || file.calls["delete"] != 1 {
		t.Fatalf("Delete calls = native %d, fallback %d; want one each", native.calls["delete"], file.calls["delete"])
	}
}

func TestCredentialStorePlatformPolicyExcludesFallbackOutsideLinux(t *testing.T) {
	for _, platform := range []string{"windows", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			native := &policyStore{sessions: map[string]string{"personal": "native-session"}}
			file := &policyStore{sessions: map[string]string{"personal": "file-session"}}
			store := NewPlatformStore(platform, native, file)

			if err := store.Save("personal", "replacement"); err != nil {
				t.Fatalf("Save: %v", err)
			}
			if got, err := store.Load("personal"); err != nil || got != "replacement" {
				t.Fatalf("Load = %q, %v; want native replacement", got, err)
			}
			if has, err := store.Has("personal"); err != nil || !has {
				t.Fatalf("Has = %t, %v; want true", has, err)
			}
			if err := store.Delete("personal"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if len(file.calls) != 0 {
				t.Errorf("file fallback operations on %s = %v, want none", platform, file.calls)
			}
			if native.calls["save"] != 1 || native.calls["load"] != 1 || native.calls["has"] != 1 || native.calls["delete"] != 1 {
				t.Errorf("native operation calls = %v, want one per operation", native.calls)
			}

			unavailableErr := ErrSecureStoreUnavailable
			unavailableNative := &policyStore{
				saveErr:   unavailableErr,
				loadErr:   unavailableErr,
				hasErr:    unavailableErr,
				deleteErr: unavailableErr,
			}
			unavailableFallback := &policyStore{}
			unavailableStore := NewPlatformStore(platform, unavailableNative, unavailableFallback)
			if err := unavailableStore.Save("personal", "session"); !errors.Is(err, unavailableErr) {
				t.Errorf("Save error = %v, want native unavailable error", err)
			}
			if _, err := unavailableStore.Load("personal"); !errors.Is(err, unavailableErr) {
				t.Errorf("Load error = %v, want native unavailable error", err)
			}
			if _, err := unavailableStore.Has("personal"); !errors.Is(err, unavailableErr) {
				t.Errorf("Has error = %v, want native unavailable error", err)
			}
			if err := unavailableStore.Delete("personal"); !errors.Is(err, unavailableErr) {
				t.Errorf("Delete error = %v, want native unavailable error", err)
			}
			if len(unavailableFallback.calls) != 0 {
				t.Errorf("file fallback operations after native unavailability on %s = %v, want none", platform, unavailableFallback.calls)
			}
		})
	}
}

func TestChangingNativeAvailabilityDoesNotMigrateCredentials(t *testing.T) {
	native := &policyStore{sessions: map[string]string{"personal": "native-session"}}
	file := &policyStore{sessions: map[string]string{"personal": "dormant-file-session"}}
	store := NewPlatformStore("linux", native, file)

	if got, err := store.Load("personal"); err != nil || got != "native-session" {
		t.Fatalf("native-tier Load = %q, %v; want native session", got, err)
	}
	if err := store.Save("personal", "replacement-native-session"); err != nil {
		t.Fatalf("save to native tier: %v", err)
	}
	if got := file.sessions["personal"]; got != "dormant-file-session" {
		t.Fatalf("dormant file credential changed to %q", got)
	}

	native.loadErr = ErrSecureStoreUnavailable
	native.saveErr = ErrSecureStoreUnavailable
	if got, err := store.Load("personal"); err != nil || got != "dormant-file-session" {
		t.Fatalf("fallback-tier Load = %q, %v; want dormant file session", got, err)
	}
	if err := store.Save("personal", "replacement-file-session"); err != nil {
		t.Fatalf("save to fallback tier: %v", err)
	}
	if got := native.sessions["personal"]; got != "replacement-native-session" {
		t.Fatalf("native credential changed to %q", got)
	}
	if got := file.sessions["personal"]; got != "replacement-file-session" {
		t.Fatalf("fallback credential = %q, want replacement-file-session", got)
	}
}
