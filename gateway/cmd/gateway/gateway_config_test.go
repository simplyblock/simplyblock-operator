// Checks that gatewayConfig (main.go) hands embedgw the Vault IAM backend, IAM
// cache, and TLS settings config.FromEnv read, without starting a server: the
// round-trip tests in main_test.go run single-account and would not notice a
// dropped field.

package main

import (
	"testing"

	"github.com/simplyblock/gateway/internal/config"
)

func TestGatewayConfigCarriesTheVaultBackendIAMCacheAndTLS(t *testing.T) {
	cfg := config.Config{
		RootAccessKeyID:     testAccessKeyID,
		RootSecretAccessKey: testSecretAccessKey,
		Port:                ":9000",
		Vault: config.Vault{
			EndpointURL:            "https://vault.example:8200",
			Namespace:              "ns",
			SecretStoragePath:      "s3/accounts",
			SecretStorageNamespace: "storage-ns",
			AuthMethod:             "approle",
			AuthNamespace:          "auth-ns",
			MountPath:              "kv-v2",
			RootToken:              "root-token",
			RoleID:                 "role-id",
			RoleSecret:             "role-secret",
			ServerCert:             "server-pem",
			ClientCert:             "client-pem",
			ClientCertKey:          "client-key-pem",
		},
		IAMCacheTTL:   120,
		IAMCachePrune: 3600,
		TLSCertFile:   "/etc/simplyblock/tls/tls.crt",
		TLSKeyFile:    "/etc/simplyblock/tls/tls.key",
	}

	got := gatewayConfig(cfg)

	for _, field := range []struct{ name, got, want string }{
		{"VaultEndpointURL", got.VaultEndpointURL, cfg.Vault.EndpointURL},
		{"VaultNamespace", got.VaultNamespace, cfg.Vault.Namespace},
		{"VaultSecretStoragePath", got.VaultSecretStoragePath, cfg.Vault.SecretStoragePath},
		{"VaultSecretStorageNamespace", got.VaultSecretStorageNamespace, cfg.Vault.SecretStorageNamespace},
		{"VaultAuthMethod", got.VaultAuthMethod, cfg.Vault.AuthMethod},
		{"VaultAuthNamespace", got.VaultAuthNamespace, cfg.Vault.AuthNamespace},
		{"VaultMountPath", got.VaultMountPath, cfg.Vault.MountPath},
		{"VaultRootToken", got.VaultRootToken, cfg.Vault.RootToken},
		{"VaultRoleID", got.VaultRoleID, cfg.Vault.RoleID},
		{"VaultRoleSecret", got.VaultRoleSecret, cfg.Vault.RoleSecret},
		{"VaultServerCert", got.VaultServerCert, cfg.Vault.ServerCert},
		{"VaultClientCert", got.VaultClientCert, cfg.Vault.ClientCert},
		{"VaultClientCertKey", got.VaultClientCertKey, cfg.Vault.ClientCertKey},
		{"CertFile", got.CertFile, cfg.TLSCertFile},
		{"KeyFile", got.KeyFile, cfg.TLSKeyFile},
	} {
		if field.got != field.want {
			t.Errorf("%s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if got.IAMCacheTTL != cfg.IAMCacheTTL || got.IAMCachePrune != cfg.IAMCachePrune {
		t.Errorf("IAMCacheTTL/IAMCachePrune = %d/%d, want %d/%d",
			got.IAMCacheTTL, got.IAMCachePrune, cfg.IAMCacheTTL, cfg.IAMCachePrune)
	}
}
