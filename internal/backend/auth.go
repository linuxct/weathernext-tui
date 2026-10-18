package backend

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

var googleOAuthEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// OAuth client credentials are injected into release binaries by the CI build.
// The secret is XOR-obfuscated at build time to keep its plaintext out of the
// binary's strings; this is obfuscation, not secure secret storage.
var OAuthClientID string
var OAuthClientSecretCipher string
var OAuthClientSecretKey string

type authService struct {
	mu       sync.Mutex
	login    *loginFlow
	endpoint oauth2.Endpoint
}

type loginFlow struct {
	mu      sync.Mutex
	request string
	state   string
	status  string
	message string
	url     string
	cancel  context.CancelFunc
}

func newAuthService(_ string) *authService { return &authService{endpoint: googleOAuthEndpoint} }

func (a *authService) status(ctx context.Context) map[string]any {
	loginAvailable := oauthClientID() != "" && oauthClientSecret() != ""
	message := "Google sign-in has not been completed."
	if oauthClientID() == "" {
		message = "This build is missing the app's Google sign-in Client ID."
	} else if oauthClientSecret() == "" {
		message = "This build is missing the app's Google sign-in Client Secret."
	}
	status := map[string]any{
		"authenticated": false, "source": nil,
		"login_available": loginAvailable,
		"message":         message,
	}
	if _, err := os.Stat(oauthCredentialPath()); err == nil && oauthClientSecret() != "" {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if source, tokenErr := a.tokenSource(checkCtx); tokenErr == nil {
			if token, refreshErr := source.Token(); refreshErr == nil && token != nil && token.AccessToken != "" {
				status["authenticated"] = true
				status["source"] = "WeatherNext browser sign-in"
				status["message"] = "Google sign-in is active."
			} else {
				status["message"] = "Saved Google sign-in has expired. Sign in again."
			}
		} else {
			status["message"] = "Saved Google sign-in could not be used. Sign in again."
		}
	}
	a.mu.Lock()
	login := a.login
	a.mu.Unlock()
	if login != nil {
		status["login"] = login.snapshot()
	} else {
		status["login"] = map[string]any{"status": "idle"}
	}
	return status
}

func (a *authService) httpClient(ctx context.Context) (*http.Client, error) {
	source, err := a.tokenSource(ctx)
	if err != nil {
		return nil, err
	}
	return oauth2.NewClient(ctx, source), nil
}

func (a *authService) tokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	path := oauthCredentialPath()
	if path == "" {
		return nil, fmt.Errorf("could not find the local configuration directory")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Google sign-in is required")
		}
		return nil, fmt.Errorf("read saved Google sign-in: %w", err)
	}
	var credentials struct {
		Type         string `json:"type"`
		ClientID     string `json:"client_id"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, fmt.Errorf("saved Google sign-in is invalid: %w", err)
	}
	if credentials.Type != "authorized_user" || credentials.ClientID == "" || credentials.RefreshToken == "" {
		return nil, fmt.Errorf("saved Google sign-in is incomplete")
	}
	clientSecret := oauthClientSecret()
	if clientSecret == "" {
		return nil, fmt.Errorf("this build is missing the Google OAuth client secret")
	}
	endpoint := a.endpoint
	if endpoint.TokenURL == "" {
		endpoint = googleOAuthEndpoint
	}
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	config := &oauth2.Config{ClientID: credentials.ClientID, ClientSecret: clientSecret, Endpoint: endpoint, Scopes: []string{cloudPlatformScope}}
	return config.TokenSource(ctx, &oauth2.Token{RefreshToken: credentials.RefreshToken}), nil
}

func (a *authService) start() map[string]any {
	clientID := oauthClientID()
	clientSecret := oauthClientSecret()
	if clientID == "" || clientSecret == "" {
		return map[string]any{"status": "error", "message": "This build is missing the app's Google OAuth Client ID or Client Secret. Rebuild with both OAuth credentials."}
	}
	a.mu.Lock()
	current := a.login
	a.mu.Unlock()
	if current != nil {
		snapshot := current.snapshot()
		if snapshot["status"] == "started" || snapshot["status"] == "needs_input" {
			return snapshot
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return map[string]any{"status": "error", "message": "Could not start the local sign-in callback."}
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/oauth2/callback", port)
	state, err := randomSecret(24)
	if err != nil {
		_ = listener.Close()
		return map[string]any{"status": "error", "message": "Could not create a secure sign-in session."}
	}
	verifier, err := randomSecret(48)
	if err != nil {
		_ = listener.Close()
		return map[string]any{"status": "error", "message": "Could not create a secure sign-in session."}
	}
	endpoint := a.endpoint
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     endpoint,
		RedirectURL:  redirectURL,
		Scopes:       []string{cloudPlatformScope},
	}
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(verifier))
	flowCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	flow := &loginFlow{request: randomIDText(), state: state, status: "started", message: "Open the Google sign-in page in your browser. The local callback will finish sign-in automatically.", url: authURL, cancel: cancel}
	a.mu.Lock()
	a.login = flow
	a.mu.Unlock()
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/callback" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid sign-in state.", http.StatusBadRequest)
			return
		}
		defer func() {
			flow.cancel()
			go func() {
				shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				_ = server.Shutdown(shutdownCtx)
			}()
		}()
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			flow.complete("failed", "Google sign-in was declined or could not complete.")
			fmt.Fprintln(w, "Google sign-in did not complete. You may close this tab.")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Google did not return an authorization code.", http.StatusBadRequest)
			return
		}
		token, exchangeErr := config.Exchange(flowCtx, code, oauth2.VerifierOption(verifier))
		if exchangeErr != nil {
			message := tokenExchangeFailure(exchangeErr)
			flow.complete("failed", message)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "<!doctype html><title>WeatherNext sign-in</title><h1>Sign-in failed</h1><p>%s</p><p>Return to WeatherNext, then start a fresh sign-in.</p>", html.EscapeString(message))
			return
		}
		if token.RefreshToken == "" {
			flow.complete("failed", "Google did not issue a refresh token. Retry sign-in and approve offline access.")
			http.Error(w, "No refresh token was issued. Retry sign-in.", http.StatusBadGateway)
			return
		}
		if saveErr := saveOAuthCredentials(clientID, token.RefreshToken); saveErr != nil {
			flow.complete("failed", "Could not save Google credentials to the local WeatherNext config folder.")
			http.Error(w, "Could not save credentials. Return to WeatherNext.", http.StatusInternalServerError)
			return
		}
		flow.complete("complete", "Google sign-in completed and credentials were saved in the local WeatherNext config folder.")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>WeatherNext sign-in</title><p>Google sign-in is complete. Return to WeatherNext.</p>")
	})
	go func() { _ = server.Serve(listener) }()
	go func() {
		<-flowCtx.Done()
		if flow.snapshot()["status"] == "started" {
			flow.complete("failed", "Google sign-in expired. Start sign-in again.")
		}
		shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = server.Shutdown(shutdownCtx)
	}()
	return flow.snapshot()
}

func (a *authService) respond(params map[string]any) map[string]any {
	a.mu.Lock()
	login := a.login
	a.mu.Unlock()
	if login == nil {
		return map[string]any{"status": "idle", "message": "No Google sign-in is running."}
	}
	if stringValue(params["action"]) == "cancel" {
		login.cancel()
		login.complete("cancelled", "Sign-in cancelled.")
		return login.snapshot()
	}
	return login.snapshot()
}

func (l *loginFlow) complete(status, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status == "complete" || l.status == "failed" || l.status == "cancelled" {
		return
	}
	l.status, l.message = status, message
}

func (l *loginFlow) snapshot() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]any{"status": l.status, "flow": "browser_loopback", "request_id": l.request, "verification_url": l.url, "message": l.message,
		"instructions": "Open the displayed Google sign-in URL, approve WeatherNext access, then return here."}
}

func oauthCredentialPath() string {
	config, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(config, "weathernext", "google_credentials.json")
}

func saveOAuthCredentials(clientID, refreshToken string) error {
	path := oauthCredentialPath()
	if path == "" {
		return fmt.Errorf("could not find the local configuration directory")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(map[string]string{"type": "authorized_user", "client_id": clientID, "refresh_token": refreshToken}, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func randomSecret(length int) (string, error) {
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func randomIDText() string {
	value, err := randomSecret(16)
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return value
}

func oauthClientID() string {
	return firstString(strings.TrimSpace(os.Getenv("WEATHERNEXT_OAUTH_CLIENT_ID")), strings.TrimSpace(OAuthClientID))
}

func oauthClientSecret() string {
	if secret := strings.TrimSpace(os.Getenv("WEATHERNEXT_OAUTH_CLIENT_SECRET")); secret != "" {
		return secret
	}
	ciphertext, err := hex.DecodeString(OAuthClientSecretCipher)
	if err != nil || len(ciphertext) == 0 {
		return ""
	}
	key, err := hex.DecodeString(OAuthClientSecretKey)
	if err != nil || len(key) == 0 {
		return ""
	}
	secret := make([]byte, len(ciphertext))
	for i := range ciphertext {
		secret[i] = ciphertext[i] ^ key[i%len(key)]
	}
	return string(secret)
}

func tokenExchangeFailure(err error) string {
	var response *oauth2.RetrieveError
	if errors.As(err, &response) {
		code := safeOAuthText(response.ErrorCode)
		description := safeOAuthText(response.ErrorDescription)
		if code != "" {
			message := "Google rejected the authorization-code exchange (" + code + ")."
			if description != "" {
				message += " " + description
			}
			switch code {
			case "invalid_grant":
				message += " The code may be expired or already used, or its PKCE verifier or redirect URI may not match. Start a fresh sign-in."
			case "invalid_client", "unauthorized_client":
				message += " Confirm this build uses the OAuth Desktop client ID that opened the sign-in page."
			}
			return message
		}
		if response.Response != nil {
			return fmt.Sprintf("Google's token endpoint returned HTTP %d without an OAuth error code. Start a fresh sign-in.", response.Response.StatusCode)
		}
	}
	return "WeatherNext could not reach Google's token endpoint. Check the network connection and try a fresh sign-in."
}

func safeOAuthText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	const limit = 240
	if len([]rune(value)) > limit {
		value = string([]rune(value)[:limit])
	}
	return value
}
