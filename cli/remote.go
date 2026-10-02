package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const defaultRemoteTokenEnv = "KORA_REMOTE_TOKEN"

type remoteProfile struct {
	URL       string `json:"url"`
	Site      string `json:"site,omitempty"`
	TokenEnv  string `json:"token_env"`
	AllowHTTP bool   `json:"allow_http,omitempty"`
}

type remoteProfileFile struct {
	Current  string                   `json:"current,omitempty"`
	Profiles map[string]remoteProfile `json:"profiles"`
}

type remoteOptions struct {
	Profile   string
	URL       string
	Site      string
	TokenEnv  string
	AllowHTTP bool
	Timeout   time.Duration
	Config    string
	Out       io.Writer
	In        io.Reader
}

type remoteClient struct {
	baseURL *url.URL
	site    string
	token   string
	http    *http.Client
}

func init() {
	rootCmd.AddCommand(newRemoteCommand())
}

func newRemoteCommand() *cobra.Command {
	opts := &remoteOptions{Timeout: 30 * time.Second}
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage a remote Kora site through its API",
		Long: `Manage records and inspect schemas on a remote Kora site.

Credentials are read from an environment variable and are never stored in the
profile file. Use a least-privilege extension or delegated channel token; the
remote site applies its normal role and DocType permission checks.`,
	}
	cmd.PersistentFlags().StringVar(&opts.Profile, "profile", "", "Remote profile name (defaults to the current profile)")
	cmd.PersistentFlags().StringVar(&opts.URL, "url", "", "Kora base URL (overrides the profile)")
	cmd.PersistentFlags().StringVar(&opts.Site, "site", "", "Site name for a shared /s/<site> endpoint")
	cmd.PersistentFlags().StringVar(&opts.TokenEnv, "token-env", "", "Environment variable containing the bearer token")
	cmd.PersistentFlags().BoolVar(&opts.AllowHTTP, "allow-http", false, "Allow unencrypted HTTP for a non-local host")
	cmd.PersistentFlags().DurationVar(&opts.Timeout, "timeout", 30*time.Second, "Request timeout")
	cmd.PersistentFlags().StringVar(&opts.Config, "config", "", "Profile file path (defaults to the user config directory)")

	cmd.AddCommand(newRemoteProfileCommand(opts))
	cmd.AddCommand(newRemoteStatusCommand(opts))
	cmd.AddCommand(newRemoteSchemaCommand(opts))
	cmd.AddCommand(newRemoteRecordsCommand(opts))
	return cmd
}

func newRemoteProfileCommand(opts *remoteOptions) *cobra.Command {
	profile := &cobra.Command{Use: "profile", Short: "Manage remote connection profiles"}

	var setURL, setSite, setTokenEnv string
	var setAllowHTTP bool
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a remote profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(setURL) == "" {
				return errors.New("--url is required")
			}
			if setTokenEnv == "" {
				setTokenEnv = defaultRemoteTokenEnv
			}
			if _, err := validatedRemoteBaseURL(setURL, setAllowHTTP); err != nil {
				return err
			}
			path, err := remoteConfigPath(opts.Config)
			if err != nil {
				return err
			}
			cfg, err := loadRemoteProfiles(path)
			if err != nil {
				return err
			}
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("profile name is required")
			}
			cfg.Profiles[name] = remoteProfile{URL: strings.TrimRight(setURL, "/"), Site: strings.TrimSpace(setSite), TokenEnv: setTokenEnv, AllowHTTP: setAllowHTTP}
			if cfg.Current == "" {
				cfg.Current = name
			}
			if err := saveRemoteProfiles(path, cfg); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved remote profile %q (token from %s).\n", name, setTokenEnv)
			return err
		},
	}
	set.Flags().StringVar(&setURL, "url", "", "Kora base URL")
	set.Flags().StringVar(&setSite, "site", "", "Site name for shared-host routing")
	set.Flags().StringVar(&setTokenEnv, "token-env", defaultRemoteTokenEnv, "Environment variable containing the bearer token")
	set.Flags().BoolVar(&setAllowHTTP, "allow-http", false, "Allow HTTP for a non-local host")

	list := &cobra.Command{
		Use:   "list",
		Short: "List remote profiles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := remoteConfigPath(opts.Config)
			if err != nil {
				return err
			}
			cfg, err := loadRemoteProfiles(path)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Profiles))
			for name := range cfg.Profiles {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				p := cfg.Profiles[name]
				marker := " "
				if name == cfg.Current {
					marker = "*"
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\t%s\tsite=%s\ttoken=%s\n", marker, name, p.URL, valueOrDash(p.Site), p.TokenEnv); err != nil {
					return err
				}
			}
			return nil
		},
	}

	use := &cobra.Command{
		Use:   "use <name>",
		Short: "Select the default remote profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := remoteConfigPath(opts.Config)
			if err != nil {
				return err
			}
			cfg, err := loadRemoteProfiles(path)
			if err != nil {
				return err
			}
			if _, ok := cfg.Profiles[args[0]]; !ok {
				return fmt.Errorf("remote profile %q does not exist", args[0])
			}
			cfg.Current = args[0]
			if err := saveRemoteProfiles(path, cfg); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Using remote profile %q.\n", args[0])
			return err
		},
	}

	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a remote profile (credentials are not stored)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := remoteConfigPath(opts.Config)
			if err != nil {
				return err
			}
			cfg, err := loadRemoteProfiles(path)
			if err != nil {
				return err
			}
			if _, ok := cfg.Profiles[args[0]]; !ok {
				return fmt.Errorf("remote profile %q does not exist", args[0])
			}
			delete(cfg.Profiles, args[0])
			if cfg.Current == args[0] {
				cfg.Current = ""
			}
			return saveRemoteProfiles(path, cfg)
		},
	}

	profile.AddCommand(set, list, use, remove)
	return profile
}

func newRemoteStatusCommand(opts *remoteOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Verify the remote site and credential",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, profileName, err := opts.client()
			if err != nil {
				return err
			}
			var result any
			if err := client.do(cmd.Context(), http.MethodGet, "/api/v1/system/navigation", nil, &result); err != nil {
				return err
			}
			return writeRemoteJSON(cmd.OutOrStdout(), map[string]any{"connected": true, "profile": profileName, "site": client.site, "navigation": result})
		},
	}
}

func newRemoteSchemaCommand(opts *remoteOptions) *cobra.Command {
	schema := &cobra.Command{Use: "schema", Short: "Inspect remote DocType schemas"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List DocTypes visible to this credential",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRemoteJSON(cmd, opts, http.MethodGet, "/api/v1/system/doctypes", nil)
		},
	}
	get := &cobra.Command{
		Use:   "get <doctype>",
		Short: "Get one DocType schema",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemoteJSON(cmd, opts, http.MethodGet, "/api/v1/system/doctype/"+canonicalResourceSegment(args[0]), nil)
		},
	}
	schema.AddCommand(list, get)
	return schema
}

func newRemoteRecordsCommand(opts *remoteOptions) *cobra.Command {
	records := &cobra.Command{Use: "records", Short: "Read and write remote records using normal Kora permissions"}

	var limit, offset int
	var filters, fields, orderBy, search string
	list := &cobra.Command{
		Use:   "list <doctype>",
		Short: "List records",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values := url.Values{}
			values.Set("limit", fmt.Sprint(limit))
			values.Set("offset", fmt.Sprint(offset))
			setQuery(values, "filters", filters)
			setQuery(values, "fields", fields)
			setQuery(values, "order_by", orderBy)
			setQuery(values, "search", search)
			return runRemoteJSON(cmd, opts, http.MethodGet, "/api/v1/resource/"+canonicalResourceSegment(args[0])+"?"+values.Encode(), nil)
		},
	}
	list.Flags().IntVar(&limit, "limit", 20, "Maximum records to return")
	list.Flags().IntVar(&offset, "offset", 0, "Records to skip")
	list.Flags().StringVar(&filters, "filters", "", "Kora filter expression")
	list.Flags().StringVar(&fields, "fields", "", "JSON array of fields to return")
	list.Flags().StringVar(&orderBy, "order-by", "", "Sort expression")
	list.Flags().StringVar(&search, "search", "", "Search text")

	get := &cobra.Command{
		Use:   "get <doctype> <name>",
		Short: "Get one record",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/api/v1/resource/" + canonicalResourceSegment(args[0]) + "/" + url.PathEscape(args[1])
			return runRemoteJSON(cmd, opts, http.MethodGet, path, nil)
		},
	}

	create := newRemoteWriteCommand(opts, "create <doctype>", "Create a record", false)
	update := newRemoteWriteCommand(opts, "update <doctype> <name>", "Update a record", true)

	var yes bool
	remove := &cobra.Command{
		Use:   "delete <doctype> <name>",
		Short: "Delete a record",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("refusing to delete without --yes")
			}
			path := "/api/v1/resource/" + canonicalResourceSegment(args[0]) + "/" + url.PathEscape(args[1])
			return runRemoteJSON(cmd, opts, http.MethodDelete, path, nil)
		},
	}
	remove.Flags().BoolVar(&yes, "yes", false, "Confirm deletion")

	records.AddCommand(list, get, create, update, remove)
	return records
}

func newRemoteWriteCommand(opts *remoteOptions, use, short string, hasName bool) *cobra.Command {
	var data, file, idempotencyKey, expectedVersion string
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args: func(cmd *cobra.Command, args []string) error {
			want := 1
			if hasName {
				want = 2
			}
			return cobra.ExactArgs(want)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, err := readRemotePayload(cmd.InOrStdin(), data, file)
			if err != nil {
				return err
			}
			client, _, err := opts.client()
			if err != nil {
				return err
			}
			doctypeName := canonicalResourceSegment(args[0])
			commandName := "record.create"
			commandPayload := map[string]any{"doctype": doctypeName, "data": json.RawMessage(payload)}
			if hasName {
				commandName = "record.update"
				commandPayload["name"] = args[1]
			}
			encodedPayload, err := json.Marshal(commandPayload)
			if err != nil {
				return err
			}
			envelope, err := json.Marshal(map[string]any{
				"idempotency_key":  idempotencyKey,
				"correlation_id":   idempotencyKey,
				"expected_version": expectedVersion,
				"payload":          json.RawMessage(encodedPayload),
			})
			if err != nil {
				return err
			}
			var result any
			if err := client.do(cmd.Context(), http.MethodPost, "/api/v1/kernel/"+commandName, envelope, &result); err != nil {
				return err
			}
			return writeRemoteJSON(cmd.OutOrStdout(), result)
		},
	}
	command.Flags().StringVar(&data, "data", "", "JSON object payload")
	command.Flags().StringVarP(&file, "file", "f", "", "Read JSON from a file, or - for stdin")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries")
	if hasName {
		command.Flags().StringVar(&expectedVersion, "expected-version", "", "Reject the update if the record version has changed")
	}
	return command
}

func (o *remoteOptions) client() (*remoteClient, string, error) {
	path, err := remoteConfigPath(o.Config)
	if err != nil {
		return nil, "", err
	}
	cfg, err := loadRemoteProfiles(path)
	if err != nil {
		return nil, "", err
	}
	name := o.Profile
	if name == "" {
		name = cfg.Current
	}
	p := cfg.Profiles[name]
	if o.URL != "" {
		p.URL = o.URL
	}
	if o.Site != "" {
		p.Site = o.Site
	}
	if o.TokenEnv != "" {
		p.TokenEnv = o.TokenEnv
	}
	if o.AllowHTTP {
		p.AllowHTTP = true
	}
	if p.URL == "" {
		return nil, "", errors.New("no remote URL configured; use --url or 'kora remote profile set'")
	}
	if p.TokenEnv == "" {
		p.TokenEnv = defaultRemoteTokenEnv
	}
	token := strings.TrimSpace(os.Getenv(p.TokenEnv))
	if token == "" {
		return nil, "", fmt.Errorf("%s is not set; Kora never stores remote bearer tokens in profiles", p.TokenEnv)
	}
	base, err := validatedRemoteBaseURL(p.URL, p.AllowHTTP)
	if err != nil {
		return nil, "", err
	}
	return &remoteClient{baseURL: base, site: p.Site, token: token, http: &http.Client{Timeout: o.Timeout}}, name, nil
}

func (c *remoteClient) do(ctx context.Context, method, path string, body []byte, result any) error {
	return c.doWithHeaders(ctx, method, path, body, nil, result)
}

func (c *remoteClient) doWithHeaders(ctx context.Context, method, path string, body []byte, headers map[string]string, result any) error {
	target := *c.baseURL
	prefix := strings.TrimRight(target.Path, "/")
	if c.site != "" && !strings.HasSuffix(prefix, "/s/"+url.PathEscape(c.site)) {
		prefix += "/s/" + url.PathEscape(c.site)
	}
	pathParts := strings.SplitN(path, "?", 2)
	target.Path = prefix + pathParts[0]
	if len(pathParts) == 2 {
		target.RawQuery = pathParts[1]
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("remote request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("reading remote response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteHTTPError(resp.StatusCode, responseBody)
	}
	if result == nil || len(bytes.TrimSpace(responseBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, result); err != nil {
		return fmt.Errorf("remote returned invalid JSON: %w", err)
	}
	return nil
}

func runRemoteJSON(cmd *cobra.Command, opts *remoteOptions, method, path string, body []byte) error {
	client, _, err := opts.client()
	if err != nil {
		return err
	}
	var result any
	if err := client.do(cmd.Context(), method, path, body, &result); err != nil {
		return err
	}
	return writeRemoteJSON(cmd.OutOrStdout(), result)
}

func remoteConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if fromEnv := os.Getenv("KORA_CLI_CONFIG"); fromEnv != "" {
		return fromEnv, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding user config directory: %w", err)
	}
	return filepath.Join(dir, "kora", "remote.json"), nil
}

func loadRemoteProfiles(path string) (remoteProfileFile, error) {
	cfg := remoteProfileFile{Profiles: map[string]remoteProfile{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading remote profiles: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing remote profiles: %w", err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]remoteProfile{}
	}
	return cfg, nil
}

func saveRemoteProfiles(path string, cfg remoteProfileFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating profile directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("writing remote profiles: %w", err)
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving remote profiles: %w", err)
	}
	return nil
}

func validatedRemoteBaseURL(raw string, allowHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid remote URL %q", raw)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("remote URL must use https (or http for local development)")
	}
	host := parsed.Hostname()
	local := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	if parsed.Scheme == "http" && !local && !allowHTTP {
		return nil, errors.New("refusing unencrypted remote connection; use https or explicitly pass --allow-http")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

var nonResourceSegment = regexp.MustCompile(`[^a-z0-9]+`)

func canonicalResourceSegment(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = nonResourceSegment.ReplaceAllString(value, "-")
	return strings.Trim(value, "-")
}

func readRemotePayload(stdin io.Reader, inline, file string) ([]byte, error) {
	if inline != "" && file != "" {
		return nil, errors.New("use either --data or --file, not both")
	}
	var data []byte
	var err error
	switch {
	case inline != "":
		data = []byte(inline)
	case file == "-":
		data, err = io.ReadAll(io.LimitReader(stdin, 8<<20))
	case file != "":
		data, err = os.ReadFile(file)
	default:
		return nil, errors.New("one of --data or --file is required")
	}
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("payload must be a JSON object: %w", err)
	}
	return json.Marshal(object)
}

func remoteHTTPError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		switch value := envelope.Error.(type) {
		case string:
			message = value
		case map[string]any:
			if text, ok := value["message"].(string); ok {
				message = text
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return fmt.Errorf("remote returned %d: %s", status, message)
}

func writeRemoteJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func setQuery(values url.Values, key, value string) {
	if value != "" {
		values.Set(key, value)
	}
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
