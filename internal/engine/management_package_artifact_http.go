package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
)

const maxManagementArtifactResponse = 3 << 20

type HTTPManagementPackageArtifactResolver struct {
	base  *url.URL
	token string
	http  *http.Client
}

func NewHTTPManagementPackageArtifactResolver(baseURL, token string, client *http.Client) (*HTTPManagementPackageArtifactResolver, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base == nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !(base.Scheme == "http" && isLoopbackManagementArtifactHost(base.Hostname()))) {
		return nil, errors.New("management package artifact URL must be HTTPS, except for loopback tests")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("management package artifact token is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	} else {
		copy := *client
		client = &copy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	return &HTTPManagementPackageArtifactResolver{base: base, token: token, http: client}, nil
}

func isLoopbackManagementArtifactHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && address.IsLoopback()
}

func (r *HTTPManagementPackageArtifactResolver) ResolvePackage(ctx context.Context, artifactRef, digest string, componentReleaseIDs []string) (ManagementPackageArtifact, error) {
	if r == nil || r.base == nil || r.http == nil || ctx == nil {
		return ManagementPackageArtifact{}, errors.New("management package artifact resolver is not configured")
	}
	if len(artifactRef) < 3 || len(artifactRef) > 160 || strings.Contains(artifactRef, "..") || strings.ContainsAny(artifactRef, "/\\\r\n") {
		return ManagementPackageArtifact{}, errors.New("management package artifact reference is invalid")
	}
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return ManagementPackageArtifact{}, errors.New("management package artifact digest is invalid")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ManagementPackageArtifact{}, errors.New("management package artifact digest is invalid")
	}
	endpoint := *r.base
	endpoint.Path = path.Join(endpoint.Path, "internal", "engine", "marketplace", "releases", artifactRef)
	query := endpoint.Query()
	query.Set("digest", digest)
	for _, id := range componentReleaseIDs {
		query.Add("optional_release_id", id)
	}
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return ManagementPackageArtifact{}, fmt.Errorf("create marketplace artifact request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")
	response, err := r.http.Do(req)
	if err != nil {
		return ManagementPackageArtifact{}, fmt.Errorf("request immutable marketplace artifact: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxManagementArtifactResponse+1))
	if err != nil {
		return ManagementPackageArtifact{}, fmt.Errorf("read marketplace artifact response: %w", err)
	}
	if len(body) > maxManagementArtifactResponse {
		return ManagementPackageArtifact{}, errors.New("marketplace artifact response exceeds size limit")
	}
	if response.StatusCode != http.StatusOK {
		return ManagementPackageArtifact{}, fmt.Errorf("marketplace artifact provider returned HTTP %d", response.StatusCode)
	}
	if response.Header.Get("X-Kora-Artifact-Digest") != digest {
		return ManagementPackageArtifact{}, errors.New("marketplace artifact response digest header does not match the operation")
	}
	var result struct {
		Manifest contract.MarketplacePackageManifest `json:"manifest"`
		Files    []contract.PackFile                 `json:"files"`
		Digest   string                              `json:"digest"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ManagementPackageArtifact{}, fmt.Errorf("decode marketplace artifact response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ManagementPackageArtifact{}, errors.New("marketplace artifact response must contain exactly one JSON value")
	}
	if result.Manifest.ReleaseID != artifactRef || result.Digest != digest {
		return ManagementPackageArtifact{}, errors.New("marketplace artifact identity or digest does not match the operation")
	}
	return ManagementPackageArtifact{Manifest: result.Manifest, Files: result.Files}, nil
}

var _ ManagementPackageArtifactResolver = (*HTTPManagementPackageArtifactResolver)(nil)
