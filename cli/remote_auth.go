package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type remoteCredential struct {
	URL     string    `json:"url"`
	Site    string    `json:"site,omitempty"`
	Session string    `json:"session"`
	Expires time.Time `json:"expires_at"`
}

type remoteCredentialFile struct {
	Credentials map[string]remoteCredential `json:"credentials"`
}

func newRemoteAuthCommand(opts *remoteOptions) *cobra.Command {
	auth := &cobra.Command{Use: "auth", Short: "Sign in to a Kora site with an email magic link"}
	var email string
	login := &cobra.Command{
		Use:   "login",
		Short: "Request and redeem an emailed magic link",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, profile, base, err := opts.remoteProfile()
			if err != nil {
				return err
			}
			if email == "" {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), "Email: "); err != nil {
					return err
				}
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				email = strings.TrimSpace(line)
			}
			if !strings.Contains(email, "@") || strings.ContainsAny(email, "\r\n") {
				return errors.New("enter a valid email address")
			}
			client := &http.Client{Timeout: opts.timeout()}
			csrf, err := getRemoteCSRF(cmd.Context(), client, base, profile.Site)
			if err != nil {
				return err
			}
			requestBody, _ := json.Marshal(map[string]string{"email": email})
			responseBody, _, err := remoteUnauthenticatedJSON(cmd.Context(), client, base, profile.Site, http.MethodPost, "/api/auth/magic-link/request", csrf, requestBody)
			if err != nil {
				return fmt.Errorf("requesting sign-in link: %w", err)
			}
			var requestResult struct {
				Data struct {
					Message string `json:"message"`
				} `json:"data"`
			}
			_ = json.Unmarshal(responseBody, &requestResult)
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", valueOrDash(requestResult.Data.Message))
			fmt.Fprint(cmd.OutOrStdout(), "Paste the sign-in link from your email: ")
			linkLine, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			token, err := magicTokenFromLink(strings.TrimSpace(linkLine), base, profile.Site)
			if err != nil {
				return err
			}
			verifyBody, _ := json.Marshal(map[string]string{"token": token})
			verifiedBody, cookies, err := remoteUnauthenticatedJSON(cmd.Context(), client, base, profile.Site, http.MethodPost, "/api/auth/magic-link/verify", csrf, verifyBody)
			if err != nil {
				return fmt.Errorf("verifying sign-in link: %w", err)
			}
			session := ""
			for _, cookie := range cookies {
				if cookie.Name == "kora_sid" {
					session = cookie.Value
				}
			}
			if session == "" {
				return errors.New("Kora accepted the link but did not return a session cookie")
			}
			var verifyResult struct {
				Data struct {
					User struct {
						Email string `json:"email"`
					} `json:"user"`
					ExpiresAt string `json:"expires_at"`
				} `json:"data"`
			}
			if err := json.Unmarshal(verifiedBody, &verifyResult); err != nil {
				return fmt.Errorf("reading sign-in response: %w", err)
			}
			expires, err := time.Parse(time.RFC3339, verifyResult.Data.ExpiresAt)
			if err != nil {
				return errors.New("Kora returned an invalid session expiry")
			}
			if err := saveRemoteCredential(opts.Config, name, remoteCredential{URL: strings.TrimRight(profile.URL, "/"), Site: profile.Site, Session: session, Expires: expires}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Signed in to profile %q as %s. Session expires %s.\n", name, verifyResult.Data.User.Email, expires.Local().Format(time.RFC822))
			return err
		},
	}
	login.Flags().StringVar(&email, "email", "", "Email address (prompted when omitted)")

	logout := &cobra.Command{
		Use:   "logout",
		Short: "Revoke the saved Kora session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, profile, base, err := opts.remoteProfile()
			if err != nil {
				return err
			}
			credential, found, err := loadRemoteCredential(opts.Config, name)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("profile %q has no saved CLI session", name)
			}
			if credential.URL != strings.TrimRight(profile.URL, "/") || credential.Site != profile.Site {
				return errors.New("saved session belongs to a different URL or site; refusing to revoke it")
			}
			remote := &remoteClient{baseURL: base, site: profile.Site, token: credential.Session, http: &http.Client{Timeout: opts.timeout()}}
			if err := remote.do(cmd.Context(), http.MethodPost, "/api/auth/logout", nil, nil); err != nil {
				return err
			}
			if err := removeRemoteCredential(opts.Config, name); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Signed out of profile %q.\n", name)
			return err
		},
	}
	auth.AddCommand(login, logout)
	return auth
}

func newRemotePageManifestCommand(opts *remoteOptions) *cobra.Command {
	manifests := &cobra.Command{Use: "page-manifests", Aliases: []string{"pages"}, Short: "Read Kora page manifests"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List page manifests visible to this credential",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRemoteJSON(cmd, opts, http.MethodGet, "/api/v1/system/page-manifests", nil)
		},
	}
	get := &cobra.Command{
		Use:   "get <name>",
		Short: "Read one page manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemoteJSON(cmd, opts, http.MethodGet, "/api/v1/system/page-manifests/"+url.PathEscape(args[0]), nil)
		},
	}
	manifests.AddCommand(list, get)
	return manifests
}

func (o *remoteOptions) remoteProfile() (string, remoteProfile, *url.URL, error) {
	path, err := remoteConfigPath(o.Config)
	if err != nil {
		return "", remoteProfile{}, nil, err
	}
	cfg, err := loadRemoteProfiles(path)
	if err != nil {
		return "", remoteProfile{}, nil, err
	}
	name := o.Profile
	if name == "" {
		name = cfg.Current
	}
	profile, ok := cfg.Profiles[name]
	if !ok {
		return "", remoteProfile{}, nil, errors.New("configure a remote profile first with `kora remote profile set`")
	}
	if o.URL != "" {
		profile.URL = o.URL
	}
	if o.Site != "" {
		profile.Site = o.Site
	}
	if o.TokenEnv != "" {
		profile.TokenEnv = o.TokenEnv
	}
	if o.TenantID != "" {
		profile.TenantID = o.TenantID
	}
	if o.LegalEntityID != "" {
		profile.LegalEntityID = o.LegalEntityID
	}
	if o.AllowHTTP {
		profile.AllowHTTP = true
	}
	if profile.URL == "" {
		return "", remoteProfile{}, nil, errors.New("remote profile has no URL")
	}
	base, err := validatedRemoteBaseURL(profile.URL, profile.AllowHTTP)
	if err != nil {
		return "", remoteProfile{}, nil, err
	}
	return name, profile, base, nil
}

func (o *remoteOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 30 * time.Second
	}
	return o.Timeout
}

func remoteEndpoint(base *url.URL, site, path string) *url.URL {
	target := *base
	prefix := strings.TrimRight(target.Path, "/")
	if site != "" && !strings.HasSuffix(prefix, "/s/"+url.PathEscape(site)) {
		prefix += "/s/" + url.PathEscape(site)
	}
	target.Path = prefix + path
	return &target
}

func getRemoteCSRF(ctx context.Context, client *http.Client, base *url.URL, site string) (string, error) {
	target := remoteEndpoint(base, site, "/api/v1/ping")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("connecting to Kora: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Kora ping returned HTTP %d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "kora_csrf" {
			return cookie.Value, nil
		}
	}
	// The Engine's public ping route is intentionally outside the session/CSRF
	// middleware, so it may only set the routing cookie. The protected auth
	// endpoints use the double-submit pattern and do not need a server-side
	// token: a fresh random value is sufficient for this non-browser client.
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("creating a CSRF token: %w", err)
	}
	return hex.EncodeToString(token), nil
}

func remoteUnauthenticatedJSON(ctx context.Context, client *http.Client, base *url.URL, site, method, path, csrf string, body []byte) ([]byte, []*http.Cookie, error) {
	req, err := http.NewRequestWithContext(ctx, method, remoteEndpoint(base, site, path).String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "kora_csrf="+csrf)
	req.Header.Set("X-Kora-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("Kora request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, remoteHTTPError(resp.StatusCode, data)
	}
	return data, resp.Cookies(), nil
}

func magicTokenFromLink(raw string, base *url.URL, site string) (string, error) {
	raw = strings.TrimSpace(strings.Trim(raw, "<>\"'"))
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || !strings.HasSuffix(parsed.Path, "/workspace/auth/login") {
		return "", errors.New("paste the complete Kora sign-in link from the email")
	}
	if base == nil || !strings.EqualFold(parsed.Scheme, base.Scheme) || !strings.EqualFold(parsed.Host, base.Host) {
		return "", errors.New("the sign-in link origin does not match the configured remote profile")
	}
	if site != "" && !strings.Contains(parsed.EscapedPath(), "/s/"+url.PathEscape(site)+"/") {
		return "", errors.New("the sign-in link belongs to a different site than the configured remote profile")
	}
	token := parsed.Query().Get("magic_token")
	if len(token) != 64 {
		return "", errors.New("the pasted link does not contain a valid magic token")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return "", errors.New("the pasted link does not contain a valid magic token")
	}
	return token, nil
}

func remoteCredentialPath(explicit string) (string, error) {
	profilePath, err := remoteConfigPath(explicit)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(profilePath), "remote-auth.json"), nil
}

func loadRemoteCredential(explicit, name string) (remoteCredential, bool, error) {
	path, err := remoteCredentialPath(explicit)
	if err != nil {
		return remoteCredential{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return remoteCredential{}, false, nil
	}
	if err != nil {
		return remoteCredential{}, false, fmt.Errorf("reading saved Kora sessions: %w", err)
	}
	var file remoteCredentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		return remoteCredential{}, false, fmt.Errorf("parsing saved Kora sessions: %w", err)
	}
	credential, ok := file.Credentials[name]
	return credential, ok, nil
}

func saveRemoteCredential(explicit, name string, credential remoteCredential) error {
	path, err := remoteCredentialPath(explicit)
	if err != nil {
		return err
	}
	file := remoteCredentialFile{Credentials: map[string]remoteCredential{}}
	if data, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("parsing saved Kora sessions: %w", err)
		}
		if file.Credentials == nil {
			file.Credentials = map[string]remoteCredential{}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("reading saved Kora sessions: %w", readErr)
	}
	file.Credentials[name] = credential
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating credential directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("writing saved Kora sessions: %w", err)
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving Kora session: %w", err)
	}
	return nil
}

func removeRemoteCredential(explicit, name string) error {
	path, err := remoteCredentialPath(explicit)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading saved Kora sessions: %w", err)
	}
	var file remoteCredentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parsing saved Kora sessions: %w", err)
	}
	delete(file.Credentials, name)
	if len(file.Credentials) == 0 {
		return os.Remove(path)
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		return fmt.Errorf("updating saved Kora sessions: %w", err)
	}
	return os.Chmod(path, 0600)
}
