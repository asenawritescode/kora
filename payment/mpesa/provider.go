package mpesa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/netguard"
	"github.com/asenawritescode/kora/payment"
)

// ErrDisabled is returned when the site lacks the M-Pesa secrets.
var ErrDisabled = errors.New("mpesa: provider disabled, secrets missing")

const (
	sandboxURL    = "https://sandbox.safaricom.co.ke"
	productionURL = "https://api.safaricom.co.ke"
)

var requiredSecrets = []string{"mpesa_consumer_key", "mpesa_consumer_secret", "mpesa_shortcode", "mpesa_passkey", "mpesa_callback_token"}

// SecretGetter reads per-site secrets (satisfied by *secret.Store).
type SecretGetter interface {
	Get(site, key string) (string, error)
}

// Config wires a per-site Provider. BaseURL and Client are injectable for tests.
type Config struct {
	Site         string
	Secrets      SecretGetter
	CallbackBase string // public site base URL, no trailing slash
	BaseURL      string // overrides the mpesa_environment URL when set
	Client       *http.Client
	Now          func() time.Time
}

// Provider issues M-Pesa STK push requests. It implements payment.Provider.
type Provider struct{ cfg Config }

// New builds a Provider, defaulting to a netguard-protected client.
func New(cfg Config) *Provider {
	if cfg.Client == nil {
		cfg.Client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{DialContext: netguard.PublicDialer(10*time.Second, 30*time.Second).DialContext},
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Provider{cfg: cfg}
}

// Name returns "mpesa".
func (p *Provider) Name() string { return "mpesa" }

func (p *Provider) secrets() (map[string]string, error) {
	out := map[string]string{}
	for _, k := range requiredSecrets {
		v, err := p.cfg.Secrets.Get(p.cfg.Site, k)
		if err != nil || strings.TrimSpace(v) == "" {
			return nil, ErrDisabled
		}
		out[k] = v
	}
	if env, err := p.cfg.Secrets.Get(p.cfg.Site, "mpesa_environment"); err == nil {
		out["mpesa_environment"] = strings.TrimSpace(env)
	}
	return out, nil
}

// Enabled reports whether all required secrets exist for the site.
func (p *Provider) Enabled() bool { _, err := p.secrets(); return err == nil }

// Request authenticates with Daraja and submits the STK push.
func (p *Provider) Request(ctx context.Context, in payment.Intent) (payment.Outcome, error) {
	s, err := p.secrets()
	if err != nil {
		return payment.Outcome{}, err
	}
	base := p.cfg.BaseURL
	if base == "" {
		base = sandboxURL
		if s["mpesa_environment"] == "production" {
			base = productionURL
		}
	}
	token, err := p.token(ctx, base, s)
	if err != nil {
		return payment.Outcome{}, err
	}
	ts := p.cfg.Now().UTC().Format("20060102150405")
	body, _ := json.Marshal(map[string]any{
		"BusinessShortCode": s["mpesa_shortcode"],
		"Password":          base64.StdEncoding.EncodeToString([]byte(s["mpesa_shortcode"] + s["mpesa_passkey"] + ts)),
		"Timestamp":         ts,
		"TransactionType":   "CustomerPayBillOnline",
		"Amount":            in.Amount.Round(0).IntPart(),
		"PartyA":            in.Contact,
		"PartyB":            s["mpesa_shortcode"],
		"PhoneNumber":       in.Contact,
		"CallBackURL":       p.cfg.CallbackBase + "/api/v1/webhooks/payments/mpesa/" + s["mpesa_callback_token"],
		"AccountReference":  in.ID,
		"TransactionDesc":   "Payment " + in.ID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mpesa/stkpush/v1/processrequest", strings.NewReader(string(body)))
	if err != nil {
		return payment.Outcome{}, fmt.Errorf("mpesa: build stk request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	var res struct {
		ResponseCode        string `json:"ResponseCode"`
		ResponseDescription string `json:"ResponseDescription"`
		CheckoutRequestID   string `json:"CheckoutRequestID"`
		ErrorMessage        string `json:"errorMessage"`
	}
	if err := p.do(req, &res); err != nil {
		return payment.Outcome{}, err
	}
	if res.ResponseCode == "0" {
		return payment.Outcome{State: payment.StatePending, ProviderRequestID: res.CheckoutRequestID}, nil
	}
	reason := firstNonEmpty(res.ResponseDescription, res.ErrorMessage, "mpesa rejected request")
	return payment.Outcome{State: payment.StateFailed, FailureReason: reason}, nil
}

func (p *Provider) token(ctx context.Context, base string, s map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/oauth/v1/generate?grant_type=client_credentials", nil)
	if err != nil {
		return "", fmt.Errorf("mpesa: build oauth request: %w", err)
	}
	req.SetBasicAuth(s["mpesa_consumer_key"], s["mpesa_consumer_secret"])
	var res struct {
		AccessToken string `json:"access_token"`
	}
	if err := p.do(req, &res); err != nil {
		return "", fmt.Errorf("mpesa: oauth: %w", err)
	}
	if res.AccessToken == "" {
		return "", errors.New("mpesa: oauth: empty access token")
	}
	return res.AccessToken, nil
}

// do sends req and decodes JSON; HTTP 4xx/5xx without a decodable body is an error.
func (p *Provider) do(req *http.Request, out any) error {
	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("mpesa: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("mpesa: read response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode >= 500 {
		return fmt.Errorf("mpesa: %s returned status %d", req.URL.Path, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mpesa: %s status %d, bad body: %w", req.URL.Path, resp.StatusCode, err)
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

var _ payment.Provider = (*Provider)(nil)
