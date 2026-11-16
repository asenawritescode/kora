package usage

import (
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    *Config
		wantErr bool
	}{
		{name: "off without a URL", env: map[string]string{"KORA_INTEGRATION_TOKEN": "t"}},
		{name: "default interval", env: map[string]string{"KORA_CLOUD_USAGE_URL": " http://cloud/api/cloud/usage ", "KORA_INTEGRATION_TOKEN": "t"},
			want: &Config{URL: "http://cloud/api/cloud/usage", Token: "t", Interval: 6 * time.Hour}},
		{name: "custom interval", env: map[string]string{"KORA_CLOUD_USAGE_URL": "http://c", "KORA_INTEGRATION_TOKEN": "t", "KORA_CLOUD_USAGE_INTERVAL": "2h"},
			want: &Config{URL: "http://c", Token: "t", Interval: 2 * time.Hour}},
		{name: "URL without token", env: map[string]string{"KORA_CLOUD_USAGE_URL": "http://c"}, wantErr: true},
		{name: "bad interval", env: map[string]string{"KORA_CLOUD_USAGE_URL": "http://c", "KORA_INTEGRATION_TOKEN": "t", "KORA_CLOUD_USAGE_INTERVAL": "soon"}, wantErr: true},
		{name: "interval under ten minutes", env: map[string]string{"KORA_CLOUD_USAGE_URL": "http://c", "KORA_INTEGRATION_TOKEN": "t", "KORA_CLOUD_USAGE_INTERVAL": "1m"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadConfig(func(key string) string { return tt.env[key] })
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("config = %+v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}
