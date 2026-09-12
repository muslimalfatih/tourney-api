package config

import "testing"

// Load reads os.Getenv directly (via caarlos0/env), so these tests set and
// restore the process environment rather than injecting a fake — the
// alternative would be a second, parallel way to configure the app that only
// tests use, which is exactly the kind of drift this package cannot afford.
func withEnv(t *testing.T, kv map[string]string, fn func()) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
	fn()
}

const validJWT = "test-secret-at-least-16-chars"
const validPepper = "test-pepper-at-least-16-chars"

func TestLoad_RefusesE2ETestModeInProduction(t *testing.T) {
	withEnv(t, map[string]string{
		"DATABASE_URL":                "postgres://x",
		"JWT_SECRET":                  validJWT,
		"AUTH_PASSWORD_LOGIN_ENABLED": "true",
		"APP_ENV":                     "production",
		"E2E_TEST_MODE":               "true",
	}, func() {
		if _, err := Load(); err == nil {
			t.Fatal("E2E_TEST_MODE=true with APP_ENV=production should refuse to load")
		}
	})
}

func TestLoad_AllowsE2ETestModeOutsideProduction(t *testing.T) {
	withEnv(t, map[string]string{
		"DATABASE_URL":  "postgres://x",
		"JWT_SECRET":    validJWT,
		"OTP_PEPPER":    validPepper,
		"APP_ENV":       "development",
		"E2E_TEST_MODE": "true",
	}, func() {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.E2ETestMode {
			t.Error("E2ETestMode = false, want true")
		}
	})
}

func TestLoad_RequiresOTPPepperWhenPasswordLoginDisabled(t *testing.T) {
	withEnv(t, map[string]string{
		"DATABASE_URL":                "postgres://x",
		"JWT_SECRET":                  validJWT,
		"AUTH_PASSWORD_LOGIN_ENABLED": "false",
		"OTP_PEPPER":                  "",
	}, func() {
		if _, err := Load(); err == nil {
			t.Fatal("no OTP_PEPPER with password login disabled should refuse to load")
		}
	})
}

func TestLoad_OTPPepperNotRequiredWhenPasswordLoginEnabled(t *testing.T) {
	withEnv(t, map[string]string{
		"DATABASE_URL":                "postgres://x",
		"JWT_SECRET":                  validJWT,
		"AUTH_PASSWORD_LOGIN_ENABLED": "true",
		"OTP_PEPPER":                  "",
	}, func() {
		if _, err := Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
}

func TestLoad_MigrationURLFallsBackToDatabaseURL(t *testing.T) {
	withEnv(t, map[string]string{
		"DATABASE_URL":                "postgres://runtime",
		"MIGRATION_DATABASE_URL":      "",
		"JWT_SECRET":                  validJWT,
		"AUTH_PASSWORD_LOGIN_ENABLED": "true",
	}, func() {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MigrationDatabaseURL != cfg.DatabaseURL {
			t.Errorf("MigrationDatabaseURL = %q, want it to fall back to DatabaseURL %q",
				cfg.MigrationDatabaseURL, cfg.DatabaseURL)
		}
	})
}
