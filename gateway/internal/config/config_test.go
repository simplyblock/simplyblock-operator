package config

import (
	"os"
	"testing"
	"time"
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
		envObjectStore, envNamespace,
		envBucketStorageClass, envBucketCapacityBytes, envBucketTimeout,
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

// setRequiredEnv sets every variable FromEnv has no default for.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envRootDir, "/data")
	t.Setenv(envRootAccessKeyID, "access")
	t.Setenv(envRootSecretKey, "secret")
	t.Setenv(envObjectStore, "team-a-store")
	t.Setenv(envNamespace, "simplyblock")
	t.Setenv(envBucketStorageClass, "simplyblock-pnfs")
	t.Setenv(envBucketCapacityBytes, "10737418240")
}

func TestFromEnvRequiresRootDirCredentialsAndBucketSettings(t *testing.T) {
	clearGatewayEnv(t)

	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv with nothing set succeeded, want an error naming the missing variables")
	}
}

func TestFromEnvDefaultsPortBucketLinksAndFsync(t *testing.T) {
	clearGatewayEnv(t)
	setRequiredEnv(t)

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
	setRequiredEnv(t)
	t.Setenv(envRootAccessKeyID, "")
	t.Setenv(envRootSecretKey, "")
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
	setRequiredEnv(t)
	t.Setenv(envFsync, "not-a-bool")

	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv with an invalid GATEWAY_FSYNC succeeded, want an error rather than a silent fallback")
	}
}

func TestFromEnvHonorsExplicitFalse(t *testing.T) {
	clearGatewayEnv(t)
	setRequiredEnv(t)
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

func TestFromEnvReadsTheBucketSettings(t *testing.T) {
	clearGatewayEnv(t)
	setRequiredEnv(t)

	got, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.ObjectStore != "team-a-store" || got.Namespace != "simplyblock" {
		t.Errorf("ObjectStore/Namespace = %q/%q, want team-a-store/simplyblock", got.ObjectStore, got.Namespace)
	}
	if got.BucketStorageClass != "simplyblock-pnfs" || got.BucketCapacityBytes != 10737418240 {
		t.Errorf("BucketStorageClass/BucketCapacityBytes = %q/%d, want simplyblock-pnfs/10737418240",
			got.BucketStorageClass, got.BucketCapacityBytes)
	}
	if got.BucketTimeout != defaultBucketTimeout {
		t.Errorf("BucketTimeout = %v, want the default %v", got.BucketTimeout, defaultBucketTimeout)
	}
}

func TestFromEnvHonorsAnExplicitBucketTimeout(t *testing.T) {
	clearGatewayEnv(t)
	setRequiredEnv(t)
	t.Setenv(envBucketTimeout, "90s")

	got, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got.BucketTimeout != 90*time.Second {
		t.Errorf("BucketTimeout = %v, want 90s", got.BucketTimeout)
	}
}

func TestFromEnvRejectsAnInvalidBucketCapacityOrTimeout(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{envBucketCapacityBytes, "ten-gigabytes"},
		{envBucketCapacityBytes, "0"},
		{envBucketTimeout, "soon"},
		{envBucketTimeout, "0s"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearGatewayEnv(t)
			setRequiredEnv(t)
			t.Setenv(tc.key, tc.value)

			if _, err := FromEnv(); err == nil {
				t.Fatalf("FromEnv with %s=%q succeeded, want an error", tc.key, tc.value)
			}
		})
	}
}
