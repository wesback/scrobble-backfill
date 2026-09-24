package lastfm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthenticatePrintsAuthorizationURLWhenBrowserOpenFails(t *testing.T) {
	client, server := newAuthenticationTestClient(t, `{"session":{"name":"alice","key":"session-key"}}`)
	defer server.Close()
	client.OpenURL = func(string) error {
		return errors.New("browser unavailable")
	}

	var out bytes.Buffer
	session, err := client.Authenticate(context.Background(), &out, strings.NewReader("\n"))
	if err != nil {
		t.Fatalf("Authenticate() error = %v, want success after session authorization", err)
	}
	if session != (Session{Name: "alice", Key: "session-key"}) {
		t.Fatalf("Authenticate() session = %#v, want alice session", session)
	}
	authorizationURL, err := client.AuthorizationURL("login-token")
	if err != nil {
		t.Fatalf("AuthorizationURL() error = %v", err)
	}
	if !strings.Contains(out.String(), "Could not open Last.fm's authorization page automatically") {
		t.Fatalf("output = %q, want manual authorization instructions", out.String())
	}
	if !strings.Contains(out.String(), authorizationURL) {
		t.Fatalf("output = %q, want full authorization URL %q", out.String(), authorizationURL)
	}
}

func TestAuthenticateContinuesWhenBrowserOpenDiagnosticFails(t *testing.T) {
	client, server := newAuthenticationTestClient(t, `{"session":{"name":"alice","key":"session-key"}}`)
	defer server.Close()
	client.OpenURL = func(string) error {
		return errors.New("browser unavailable")
	}
	var diagnosticRecorded bool
	client.Logger = diagnosticLoggerFunc(func(event string, fields map[string]any) error {
		diagnosticRecorded = event == "lastfm.authorization.browser_open_failed" &&
			fields["error"] == "browser unavailable"
		return errors.New("diagnostic writer unavailable")
	})

	session, err := client.Authenticate(context.Background(), &bytes.Buffer{}, strings.NewReader("\n"))
	if err != nil {
		t.Fatalf("Authenticate() error = %v, want success despite diagnostic write failure", err)
	}
	if session != (Session{Name: "alice", Key: "session-key"}) {
		t.Fatalf("Authenticate() session = %#v, want alice session", session)
	}
	if !diagnosticRecorded {
		t.Fatal("Authenticate() did not record the browser opener error")
	}
}

func TestAuthenticateDoesNotPrintAuthorizationURLWhenBrowserOpens(t *testing.T) {
	client, server := newAuthenticationTestClient(t, `{"session":{"name":"alice","key":"session-key"}}`)
	defer server.Close()
	var openedURL string
	client.OpenURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	var out bytes.Buffer
	if _, err := client.Authenticate(context.Background(), &out, strings.NewReader("\n")); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !strings.Contains(out.String(), "Last.fm's authorization page was opened in your browser.") {
		t.Fatalf("output = %q, want existing browser-open message", out.String())
	}
	authorizationURL, err := client.AuthorizationURL("login-token")
	if err != nil {
		t.Fatalf("AuthorizationURL() error = %v", err)
	}
	if openedURL != authorizationURL {
		t.Fatalf("opened URL = %q, want %q", openedURL, authorizationURL)
	}
	if strings.Contains(out.String(), authorizationURL) {
		t.Fatalf("output contains raw authorization URL on browser success: %q", out.String())
	}
}

func TestAuthenticateReturnsSessionErrorAfterBrowserOpenFails(t *testing.T) {
	client, server := newAuthenticationTestClient(t, `{"error":14,"message":"Invalid token"}`)
	defer server.Close()
	client.OpenURL = func(string) error {
		return errors.New("browser unavailable")
	}

	_, err := client.Authenticate(context.Background(), &bytes.Buffer{}, strings.NewReader("\n"))
	const want = "request Last.fm session: Last.fm API error 14: Invalid token"
	if err == nil || err.Error() != want {
		t.Fatalf("Authenticate() error = %v, want %q", err, want)
	}
}

func newAuthenticationTestClient(t *testing.T, sessionResponse string) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("request method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		switch r.PostForm.Get("method") {
		case "auth.getToken":
			fmt.Fprint(w, `{"token":"login-token"}`)
		case "auth.getSession":
			fmt.Fprint(w, sessionResponse)
		default:
			http.Error(w, "unexpected Last.fm method", http.StatusBadRequest)
		}
	}))
	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	return client, server
}

type diagnosticLoggerFunc func(string, map[string]any) error

func (f diagnosticLoggerFunc) Debug(event string, fields map[string]any) error {
	return f(event, fields)
}
