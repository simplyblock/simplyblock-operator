package net

import (
	"strings"
	"testing"
)

func TestValidateExternalURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string // substring; empty means no error expected
	}{
		{name: "empty is ok", url: ""},
		{name: "valid https public IP", url: "https://8.8.8.8:8200"},
		{name: "valid https public IP with path", url: "https://1.1.1.1/bucket"},
		{name: "http rejected", url: "http://vault.example.com", wantErr: "scheme must be https"},
		{name: "no scheme rejected", url: "vault.example.com", wantErr: "scheme must be https"},
		{name: "loopback", url: "https://127.0.0.1", wantErr: "restricted IP"},
		{name: "link-local IMDS", url: "https://169.254.169.254", wantErr: "restricted IP"},
		{name: "IPv6 loopback", url: "https://[::1]", wantErr: "restricted IP"},
		{name: "IPv6 link-local", url: "https://[fe80::1]", wantErr: "restricted IP"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExternalURL(tc.url)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error %q to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
