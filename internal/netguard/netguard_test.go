package netguard

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestIsPrivateHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"api.localhost", true},
		{"db.internal", true},
		{"printer.local", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.5", true},
		{"192.168.1.10", true},
		{"169.254.169.254", true},
		{"api.safaricom.co.ke", false},
		{"8.8.8.8", false},
	}
	for _, tt := range tests {
		if got := IsPrivateHost(tt.host); got != tt.want {
			t.Errorf("IsPrivateHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

func TestCheckURL(t *testing.T) {
	resolver := fakeResolver{
		"hooks.example.com":   {"93.184.216.34"},
		"rebind.example.com":  {"93.184.216.34", "10.0.0.7"},
		"metadata.example.io": {"169.254.169.254"},
	}
	tests := []struct {
		url        string
		wantErr    bool
		disallowed bool
	}{
		{url: "https://hooks.example.com/kora", wantErr: false},
		{url: "http://93.184.216.34:8080/hook", wantErr: false},
		{url: "https://rebind.example.com/", wantErr: true, disallowed: true},
		{url: "https://metadata.example.io/", wantErr: true, disallowed: true},
		{url: "http://169.254.169.254/latest/meta-data", wantErr: true, disallowed: true},
		{url: "http://127.0.0.1:8000/api", wantErr: true, disallowed: true},
		{url: "http://[::1]/", wantErr: true, disallowed: true},
		{url: "http://0.0.0.0/", wantErr: true, disallowed: true},
		{url: "http://100.64.1.1/", wantErr: true, disallowed: true},
		{url: "http://localhost:3000/", wantErr: true, disallowed: true},
		{url: "https://db.internal/", wantErr: true, disallowed: true},
		{url: "ftp://hooks.example.com/", wantErr: true},
		{url: "/relative/path", wantErr: true},
		{url: "https://unknown.example.com/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := CheckURL(context.Background(), tt.url, resolver)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
			if tt.disallowed && !errors.Is(err, ErrDisallowedDestination) {
				t.Fatalf("CheckURL(%q) error = %v, want ErrDisallowedDestination", tt.url, err)
			}
		})
	}
}

func TestPublicDialerRefusesLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := PublicDialer(time.Second, 0).DialContext(context.Background(), "tcp", listener.Addr().String())
	if err == nil {
		conn.Close()
		t.Fatal("PublicDialer connected to a loopback listener")
	}
	if !errors.Is(err, ErrDisallowedDestination) {
		t.Fatalf("dial error = %v, want ErrDisallowedDestination", err)
	}
}
