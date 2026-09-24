// Package lastfm implements the small portion of the Last.fm API needed by
// the command-line account flow.
package lastfm

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	DefaultAPIURL    = "https://ws.audioscrobbler.com/2.0/"
	AuthorizationURL = "https://www.last.fm/api/auth/"
	APIKeyEnv        = "RESCOBBLE_LASTFM_API_KEY"
	APISecretEnv     = "RESCOBBLE_LASTFM_API_SECRET"
	requestTimeout   = 30 * time.Second
)

var (
	ErrInvalidClient = errors.New("Last.fm client is not configured")
)

// APIError describes an error returned by the Last.fm API.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return "Last.fm API error"
	}
	return fmt.Sprintf("Last.fm API error %d: %s", e.Code, e.Message)
}

// HTTPError describes an unsuccessful HTTP response without retaining its
// body. A valid Last.fm API error envelope may be included as safe diagnostic
// details.
type HTTPError struct {
	StatusCode int
	Status     string
	APIError   *APIError
}

func (e *HTTPError) Error() string {
	message := fmt.Sprintf("Last.fm returned HTTP %s", e.Status)
	if e.APIError != nil {
		message += ": " + e.APIError.Error()
	}
	return message
}

// Client is a Last.fm API client. API credentials are supplied by the
// environment or by an injected test client and are never persisted.
type Client struct {
	APIKey     string
	APISecret  string
	BaseURL    string
	HTTPClient *http.Client
	OpenURL    func(string) error
	Logger     DiagnosticLogger
}

// DiagnosticLogger records debug events for non-fatal client diagnostics.
type DiagnosticLogger interface {
	Debug(string, map[string]any) error
}

// Session is the non-secret metadata returned by auth.getSession. Key is
// deliberately kept separate from this type so callers must explicitly store
// it through the credential boundary.
type Session struct {
	Name string
	Key  string
}

// NewClient creates a client using the production Last.fm API endpoint.
func NewClient(apiKey, apiSecret string) *Client {
	return &Client{
		APIKey:     apiKey,
		APISecret:  apiSecret,
		BaseURL:    DefaultAPIURL,
		HTTPClient: &http.Client{Timeout: requestTimeout},
		OpenURL:    openURL,
	}
}

// NewClientFromEnv creates a client from process environment configuration.
func NewClientFromEnv() *Client {
	return NewClient(os.Getenv(APIKeyEnv), os.Getenv(APISecretEnv))
}

// Authenticate performs Last.fm's auth.getToken -> browser authorization ->
// auth.getSession flow. If the authorization URL cannot be opened in the
// user's default browser, it is printed for manual authorization.
func (c *Client) Authenticate(ctx context.Context, out io.Writer, in io.Reader) (Session, error) {
	if err := c.validate(); err != nil {
		return Session{}, err
	}
	tokenResponse, err := c.callJSON(ctx, "auth.getToken", nil, "")
	if err != nil {
		return Session{}, fmt.Errorf("request Last.fm authentication token: %w", err)
	}
	token, err := stringField(tokenResponse, "token")
	if err != nil {
		return Session{}, fmt.Errorf("Last.fm authentication token response: %w", err)
	}

	authorizationURL, err := c.AuthorizationURL(token)
	if err != nil {
		return Session{}, fmt.Errorf("create Last.fm authorization URL: %w", err)
	}
	opener := c.OpenURL
	if opener == nil {
		opener = openURL
	}
	if err := opener(authorizationURL); err != nil {
		if out != nil {
			fmt.Fprintf(out, "Could not open Last.fm's authorization page automatically. Open this URL manually, authorize the displayed token, then press Enter:\n%s\n", authorizationURL)
		}
		if c.Logger != nil {
			_ = c.Logger.Debug("lastfm.authorization.browser_open_failed", map[string]any{"error": err.Error()})
		}
	} else if out != nil {
		fmt.Fprintf(out, "Last.fm's authorization page was opened in your browser. Authorize the displayed token, then press Enter.\n")
	}
	if in == nil {
		return Session{}, errors.New("Last.fm authorization requires confirmation on standard input")
	}
	if _, err := bufio.NewReader(in).ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
		return Session{}, fmt.Errorf("wait for Last.fm authorization: %w", err)
	}

	sessionResponse, err := c.callJSON(ctx, "auth.getSession", map[string]string{"token": token}, "")
	if err != nil {
		return Session{}, fmt.Errorf("request Last.fm session: %w", err)
	}
	sessionObject, ok := sessionResponse["session"].(map[string]any)
	if !ok {
		return Session{}, errors.New("Last.fm session response did not contain a session")
	}
	name, err := stringField(sessionObject, "name")
	if err != nil {
		return Session{}, fmt.Errorf("Last.fm session response: %w", err)
	}
	key, err := stringField(sessionObject, "key")
	if err != nil {
		return Session{}, fmt.Errorf("Last.fm session response: %w", err)
	}
	return Session{Name: name, Key: key}, nil
}

// AuthorizationURL returns the official Last.fm authorization URL, which
// contains the application key and should only be printed for manual
// authorization when opening the browser fails.
func (c *Client) AuthorizationURL(token string) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", errors.New("Last.fm authorization token must not be empty")
	}
	query := url.Values{"api_key": {c.APIKey}, "token": {token}}
	return AuthorizationURL + "?" + query.Encode(), nil
}

func openURL(rawURL string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{rawURL}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		command = "xdg-open"
		args = []string{rawURL}
	}
	return exec.Command(command, args...).Run()
}

// Call performs a signed Last.fm API request. params must contain only
// method-specific parameters; api_key, api_sig, format, and sk are added by
// the client. Parameters are sent in an HTTP form body so session credentials
// are not exposed in request URLs.
func (c *Client) Call(ctx context.Context, method string, params map[string]string, sessionKey string) (map[string]any, error) {
	return c.callJSON(ctx, method, params, sessionKey)
}

func (c *Client) callJSON(ctx context.Context, method string, params map[string]string, sessionKey string) (map[string]any, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	method = strings.TrimSpace(method)
	if method == "" {
		return nil, errors.New("Last.fm method must not be empty")
	}
	values := make(map[string]string, len(params)+4)
	for key, value := range params {
		values[key] = value
	}
	values["api_key"] = c.APIKey
	values["method"] = method
	if strings.TrimSpace(sessionKey) != "" {
		values["sk"] = sessionKey
	}
	values["api_sig"] = signature(values, c.APISecret)
	values["format"] = "json"

	endpoint := strings.TrimRight(c.BaseURL, "/")
	if endpoint == "" {
		endpoint = DefaultAPIURL
	}
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse Last.fm API URL: %w", err)
	}
	form := make(url.Values, len(values))
	for key, value := range values {
		form.Set(key, value)
	}
	requestContext, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, requestURL.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create Last.fm request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send Last.fm request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read Last.fm response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		httpErr := &HTTPError{StatusCode: response.StatusCode, Status: response.Status}
		var envelope struct {
			Code    *int    `json:"error"`
			Message *string `json:"message"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Code != nil && envelope.Message != nil {
			httpErr.APIError = &APIError{
				Code:    *envelope.Code,
				Message: redactCredentials(*envelope.Message, c.APIKey, c.APISecret, sessionKey, values["api_sig"]),
			}
		}
		return nil, httpErr
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Last.fm response: %w", err)
	}
	if code, ok := payload["error"].(float64); ok {
		message, _ := payload["message"].(string)
		return nil, &APIError{
			Code:    int(code),
			Message: redactCredentials(message, c.APIKey, c.APISecret, sessionKey, values["api_sig"]),
		}
	}
	return payload, nil
}

func redactCredentials(message string, credentials ...string) string {
	secrets := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		if credential != "" {
			secrets = append(secrets, credential)
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) == len(secrets[j]) {
			return secrets[i] < secrets[j]
		}
		return len(secrets[i]) > len(secrets[j])
	})
	replacements := make([]string, 0, len(secrets)*2)
	for _, secret := range secrets {
		replacements = append(replacements, secret, "[redacted]")
	}
	if len(replacements) == 0 {
		return message
	}
	message = strings.NewReplacer(replacements...).Replace(message)
	for _, secret := range secrets {
		if strings.Contains(message, secret) {
			return ""
		}
	}
	return message
}

func (c *Client) validate() error {
	if c == nil || strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.APISecret) == "" {
		return ErrInvalidClient
	}
	return nil
}

func signature(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		if key == "format" || key == "api_sig" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteString(params[key])
	}
	builder.WriteString(secret)
	sum := md5.Sum([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func stringField(payload map[string]any, name string) (string, error) {
	value, ok := payload[name].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is missing", name)
	}
	return value, nil
}
