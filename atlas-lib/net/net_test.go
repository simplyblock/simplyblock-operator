package net

import (
	"strings"
	"testing"
)

func TestValidateExternalURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string // substring, where empty means no error expected
	}{
		{name: "empty is ok", url: ""},
		{name: "valid https public IP", url: "https://8.8.8.8:8200"},
		{name: "valid https public IP with path", url: "https://1.1.1.1/bucket"},
		{name: "valid http public IP", url: "http://8.8.8.8:9000"},
		{name: "no scheme rejected", url: "vault.example.com", wantErr: "scheme must be http or https"},
		{name: "ftp rejected", url: "ftp://vault.example.com", wantErr: "scheme must be http or https"},
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

func TestValidateExternalHostPort(t *testing.T) {
	tests := []struct {
		name     string
		hostport string
		wantErr  string // substring, where empty means no error expected
	}{
		{name: "empty is ok", hostport: ""},
		{name: "valid public IP", hostport: "8.8.8.8:5696"},
		{name: "no port rejected", hostport: "kmip.example.com", wantErr: "malformed host:port"},
		{name: "loopback", hostport: "127.0.0.1:5696", wantErr: "restricted IP"},
		{name: "link-local IMDS", hostport: "169.254.169.254:5696", wantErr: "restricted IP"},
		{name: "IPv6 loopback", hostport: "[::1]:5696", wantErr: "restricted IP"},
		{name: "IPv6 link-local", hostport: "[fe80::1]:5696", wantErr: "restricted IP"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExternalHostPort(tc.hostport)
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
