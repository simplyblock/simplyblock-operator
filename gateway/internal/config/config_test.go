package config

import (
	"os"
	"testing"
)

// clearGatewayEnv clears every variable FromEnv reads, restoring each to its
// value from before the test once it ends, so a variable one test sets does
// not leak into the next.
func clearGatewayEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		envRootDir, envPort,
		envRootAccessKeyID, envRootAccessKeyIDAlt,
		envRootSecretKey, envRootSecretKeyAlt,
		envBucketLinks, envFsync,
	} {
		previous, had := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unsetting %s: %v", key, err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, previous)
			}
		})
	}
}

func TestFromEnvRequiresRootDirAndCredentials(t *testing.T) {
	clearGatewayEnv(t)

	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv with nothing set succeeded, want an error naming the missing variables")
	}
}

func TestFromEnvDefaultsPortBucketLinksAndFsync(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv(envRootDir, "/data")
	t.Setenv(envRootAccessKeyID, "access")
	t.Setenv(envRootSecretKey, "secret")

	got, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.Port != defaultPort {
		t.Errorf("Port = %q, want the default %q", got.Port, defaultPort)
	}
	if got.BucketLinks != defaultBucketLinks {
		t.Errorf("BucketLinks = %v, want the default %v", got.BucketLinks, defaultBucketLinks)
	}
	if got.Fsync != defaultFsync {
		t.Errorf("Fsync = %v, want the default %v", got.Fsync, defaultFsync)
	}
}

func TestFromEnvAcceptsVersitygwsOwnAltEnvNames(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv(envRootDir, "/data")
	t.Setenv(envRootAccessKeyIDAlt, "access")
	t.Setenv(envRootSecretKeyAlt, "secret")

	got, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.RootAccessKeyID != "access" || got.RootSecretAccessKey != "secret" {
		t.Errorf("credentials = %q/%q, want access/secret read from the alt env names",
			got.RootAccessKeyID, got.RootSecretAccessKey)
	}
}

func TestFromEnvRejectsAnInvalidBoolRatherThanDefaulting(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv(envRootDir, "/data")
	t.Setenv(envRootAccessKeyID, "access")
	t.Setenv(envRootSecretKey, "secret")
	t.Setenv(envFsync, "not-a-bool")

	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv with an invalid GATEWAY_FSYNC succeeded, want an error rather than a silent fallback")
	}
}

func TestFromEnvHonorsExplicitFalse(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv(envRootDir, "/data")
	t.Setenv(envRootAccessKeyID, "access")
	t.Setenv(envRootSecretKey, "secret")
	t.Setenv(envBucketLinks, "false")
	t.Setenv(envFsync, "false")

	got, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.BucketLinks {
		t.Error("BucketLinks = true, want false: an explicit false must not be overridden by the default")
	}
	if got.Fsync {
		t.Error("Fsync = true, want false: an explicit false must not be overridden by the default")
	}
}
