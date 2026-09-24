package lastfm

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestCallJSONIncludesAPIErrorForNon2xxResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":10,"message":"Invalid API key"}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	_, err := client.Call(context.Background(), "test.method", nil, "")
	if err == nil {
		t.Fatal("Call() error = nil, want HTTP and API error details")
	}
	const want = "Last.fm returned HTTP 403 Forbidden: Last.fm API error 10: Invalid API key"
	if err.Error() != want {
		t.Fatalf("Call() error = %q, want %q", err, want)
	}
}

func TestCallJSONNon2xxWithoutValidAPIErrorDoesNotExposeBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"error":10,"message":"body-sentinel"`},
		{name: "non-JSON", body: "remote-body-sentinel"},
		{name: "unrelated JSON", body: `{"detail":"remote-body-sentinel"}`},
		{name: "invalid error envelope", body: `{"error":"10","message":"remote-body-sentinel"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()

			client := NewClient("app-key", "app-secret")
			client.BaseURL = server.URL
			_, err := client.Call(context.Background(), "test.method", nil, "")
			if err == nil {
				t.Fatal("Call() error = nil, want HTTP error")
			}
			if !strings.Contains(err.Error(), "HTTP 502 Bad Gateway") {
				t.Errorf("Call() error = %q, want HTTP status", err)
			}
			if strings.Contains(err.Error(), test.body) || strings.Contains(err.Error(), "remote-body-sentinel") {
				t.Errorf("Call() error exposes response body %q: %v", test.body, err)
			}
		})
	}
}

func TestCallJSONRedactsCredentialsFromNon2xxAPIError(t *testing.T) {
	const (
		apiKey     = "api-key-credential"
		apiSecret  = "api-secret-credential"
		sessionKey = "session-key-credential"
	)
	var requestSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		requestSignature = r.PostForm.Get("api_sig")
		message := fmt.Sprintf("Invalid %s; credentials: %s %s %s", apiKey, apiSecret, sessionKey, requestSignature)
		w.WriteHeader(http.StatusForbidden)
		if err := json.NewEncoder(w).Encode(map[string]any{"error": 10, "message": message}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := NewClient(apiKey, apiSecret)
	client.BaseURL = server.URL
	_, err := client.Call(context.Background(), "test.method", nil, sessionKey)
	if err == nil {
		t.Fatal("Call() error = nil, want HTTP API error")
	}
	if requestSignature == "" {
		t.Fatal("request did not contain a signature")
	}
	for name, credential := range map[string]string{
		"API key":           apiKey,
		"API secret":        apiSecret,
		"session key":       sessionKey,
		"request signature": requestSignature,
	} {
		if strings.Contains(err.Error(), credential) {
			t.Errorf("Call() error exposes %s %q: %v", name, credential, err)
		}
	}
	if !strings.Contains(err.Error(), "Last.fm API error 10") {
		t.Errorf("Call() error = %q, want parsed API error details", err)
	}
	if !strings.Contains(err.Error(), "Invalid [redacted]") {
		t.Errorf("Call() error = %q, want credential visibly masked in the diagnostic", err)
	}
}

func TestCallJSONRetainsAPIErrorForSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"error":14,"message":"Invalid token"}`)
	}))
	defer server.Close()

	client := NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	_, err := client.Call(context.Background(), "test.method", nil, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Call() error = %v, want *APIError", err)
	}
	if apiErr.Code != 14 || apiErr.Message != "Invalid token" {
		t.Fatalf("APIError = %#v, want code 14 and message Invalid token", apiErr)
	}
}

func TestRedactCredentialsOmitsMessageIfMarkerWouldExposeCredential(t *testing.T) {
	if got := redactCredentials("echoed-api-key", "api-key", "[redacted]"); got != "" {
		t.Fatalf("redactCredentials() = %q, want empty message to avoid exposing a credential", got)
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
