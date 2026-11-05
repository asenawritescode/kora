package auth

import (
	"reflect"
	"testing"
)

func TestOIDCProviderNormalizeAndValidate(t *testing.T) {
	cases := []struct {
		name      string
		in        OIDCProvider
		wantErr   bool
		wantScope []string
	}{
		{name: "https issuer with defaults", in: OIDCProvider{ID: "kora-cloud", Issuer: "https://id.kora.cloud/", ClientID: "studio"}, wantScope: []string{"openid", "email", "profile"}},
		{name: "openid always first and deduplicated", in: OIDCProvider{ID: "google", Issuer: "https://accounts.google.com", ClientID: "c", Scopes: []string{"email", "openid", "email"}}, wantScope: []string{"openid", "email"}},
		{name: "loopback http allowed for development", in: OIDCProvider{ID: "dev", Issuer: "http://127.0.0.1:9000", ClientID: "c"}, wantScope: []string{"openid", "email", "profile"}},
		{name: "plain http rejected", in: OIDCProvider{ID: "dev", Issuer: "http://idp.example.com", ClientID: "c"}, wantErr: true},
		{name: "issuer with query rejected", in: OIDCProvider{ID: "x", Issuer: "https://idp.example.com?a=1", ClientID: "c"}, wantErr: true},
		{name: "missing client id rejected", in: OIDCProvider{ID: "x", Issuer: "https://idp.example.com"}, wantErr: true},
		{name: "invalid id rejected", in: OIDCProvider{ID: "Bad ID", Issuer: "https://idp.example.com", ClientID: "c"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.in.Normalize()
			err := p.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !reflect.DeepEqual(p.Scopes, tc.wantScope) {
				t.Fatalf("scopes = %v, want %v", p.Scopes, tc.wantScope)
			}
			if p.ClientSecretKey != ClientSecretKeyFor(p.ID) || p.DisplayName == "" {
				t.Fatalf("defaults not applied: %+v", p)
			}
		})
	}
}
