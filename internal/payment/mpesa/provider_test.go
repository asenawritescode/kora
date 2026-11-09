package mpesa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/payment"
)

type fakeSecrets map[string]string

func (f fakeSecrets) Get(_, k string) (string, error) {
	if v, ok := f[k]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

func fullSecrets() fakeSecrets {
	return fakeSecrets{"mpesa_consumer_key": "ck", "mpesa_consumer_secret": "cs", "mpesa_shortcode": "174379",
		"mpesa_passkey": "pk", "mpesa_callback_token": "tok"}
}

func fake(t *testing.T, oauthStatus int, stk map[string]any) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v1/generate":
			if u, p, ok := r.BasicAuth(); !ok || u != "ck" || p != "cs" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(oauthStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "T"})
		case "/mpesa/stkpush/v1/processrequest":
			if r.Header.Get("Authorization") != "Bearer T" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(stk)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newProv(srv *httptest.Server, s fakeSecrets) *Provider {
	return New(Config{Site: "s", Secrets: s, CallbackBase: "https://x.test", BaseURL: srv.URL, Client: srv.Client(),
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }})
}

var intent = payment.Intent{ID: "01ID", Amount: decimal.RequireFromString("150.60"), Contact: "254700000000"}

func TestRequestSuccessPending(t *testing.T) {
	srv, got := fake(t, 200, map[string]any{"ResponseCode": "0", "CheckoutRequestID": "ws_CO_1"})
	out, err := newProv(srv, fullSecrets()).Request(context.Background(), intent)
	if err != nil || out.State != payment.StatePending || out.ProviderRequestID != "ws_CO_1" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	g := *got
	if g["Timestamp"] != "20260102030405" {
		t.Fatalf("timestamp %v", g["Timestamp"])
	}
	if want := base64.StdEncoding.EncodeToString([]byte("174379pk20260102030405")); g["Password"] != want {
		t.Fatalf("password %v", g["Password"])
	}
	if g["Amount"] != float64(151) {
		t.Fatalf("amount %v", g["Amount"])
	}
	if g["CallBackURL"] != "https://x.test/api/v1/webhooks/payments/mpesa/tok" || g["AccountReference"] != "01ID" || g["PhoneNumber"] != "254700000000" {
		t.Fatalf("body %v", g)
	}
}

func TestRequestErrorResponseFailed(t *testing.T) {
	srv, _ := fake(t, 200, map[string]any{"ResponseCode": "1", "ResponseDescription": "Insufficient"})
	out, err := newProv(srv, fullSecrets()).Request(context.Background(), intent)
	if err != nil || out.State != payment.StateFailed || out.FailureReason != "Insufficient" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestRequestOAuthFailureErrors(t *testing.T) {
	s := fullSecrets()
	s["mpesa_consumer_secret"] = "wrong"
	srv, _ := fake(t, 200, map[string]any{})
	if _, err := newProv(srv, s).Request(context.Background(), intent); err == nil {
		t.Fatal("want oauth error")
	}
}

func TestMissingSecretsDisabled(t *testing.T) {
	srv, _ := fake(t, 200, map[string]any{})
	s := fullSecrets()
	delete(s, "mpesa_passkey")
	p := newProv(srv, s)
	if p.Enabled() {
		t.Fatal("should be disabled")
	}
	if _, err := p.Request(context.Background(), intent); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err=%v", err)
	}
	if p.Name() != "mpesa" {
		t.Fatal("name")
	}
}

func TestLiveSandbox(t *testing.T) {
	ck, cs := os.Getenv("KORA_MPESA_SANDBOX_CONSUMER_KEY"), os.Getenv("KORA_MPESA_SANDBOX_CONSUMER_SECRET")
	if ck == "" || cs == "" {
		t.Skip("sandbox keys not set")
	}
	pk := os.Getenv("KORA_MPESA_SANDBOX_PASSKEY")
	s := fakeSecrets{"mpesa_consumer_key": ck, "mpesa_consumer_secret": cs, "mpesa_shortcode": "174379", "mpesa_passkey": pk, "mpesa_callback_token": "t"}
	p := New(Config{Site: "s", Secrets: s, CallbackBase: "https://example.com"})
	if _, err := p.token(context.Background(), sandboxURL, map[string]string{"mpesa_consumer_key": ck, "mpesa_consumer_secret": cs}); err != nil {
		t.Fatal(err)
	}
}
