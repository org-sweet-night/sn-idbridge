// SPDX-License-Identifier: MIT

package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	setConfigEnv(t, nil)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	if cfg.DefaultLocale != "en-US" {
		t.Fatalf("DefaultLocale = %q, want %q", cfg.DefaultLocale, "en-US")
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, 10*time.Second)
	}
	if cfg.OIDCIssuer != "http://localhost:8080" {
		t.Fatalf("OIDCIssuer = %q, want %q", cfg.OIDCIssuer, "http://localhost:8080")
	}
	if cfg.OIDCKeyID != "dev-key-1" {
		t.Fatalf("OIDCKeyID = %q, want %q", cfg.OIDCKeyID, "dev-key-1")
	}
	if cfg.AccessTokenTTL != 15*time.Minute {
		t.Fatalf("AccessTokenTTL = %s, want %s", cfg.AccessTokenTTL, 15*time.Minute)
	}
	if cfg.IDTokenTTL != 15*time.Minute {
		t.Fatalf("IDTokenTTL = %s, want %s", cfg.IDTokenTTL, 15*time.Minute)
	}
	if cfg.AuthCodeTTL != 5*time.Minute {
		t.Fatalf("AuthCodeTTL = %s, want %s", cfg.AuthCodeTTL, 5*time.Minute)
	}
	if cfg.WebBaseURL != "" {
		t.Fatalf("WebBaseURL = %q, want empty default", cfg.WebBaseURL)
	}
	if cfg.FeishuBaseURL != "https://open.feishu.cn" {
		t.Fatalf("FeishuBaseURL = %q, want default", cfg.FeishuBaseURL)
	}
	if cfg.DBPoolAcquireTimeout != 2*time.Second {
		t.Fatalf("DBPoolAcquireTimeout = %s, want %s", cfg.DBPoolAcquireTimeout, 2*time.Second)
	}
	if cfg.DBBackgroundOperationTimeout != 5*time.Minute {
		t.Fatalf("DBBackgroundOperationTimeout = %s, want %s", cfg.DBBackgroundOperationTimeout, 5*time.Minute)
	}
	if cfg.DBBackgroundMaxConcurrency != 2 {
		t.Fatalf("DBBackgroundMaxConcurrency = %d, want 2", cfg.DBBackgroundMaxConcurrency)
	}
}

func TestLoadAcceptsDatabasePoolSettings(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"DB_POOL_MAX_CONNS":               "15",
		"DB_POOL_MIN_CONNS":               "2",
		"DB_POOL_MAX_CONN_LIFETIME":       "45m",
		"DB_POOL_MAX_CONN_IDLE_TIME":      "10m",
		"DB_POOL_ACQUIRE_TIMEOUT":         "1500ms",
		"DB_BACKGROUND_OPERATION_TIMEOUT": "90s",
		"DB_BACKGROUND_MAX_CONCURRENCY":   "3",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DBPoolMaxConns != 15 || cfg.DBPoolMinConns != 2 {
		t.Fatalf("pool bounds = %d/%d, want 15/2", cfg.DBPoolMaxConns, cfg.DBPoolMinConns)
	}
	if cfg.DBPoolMaxLifetime != 45*time.Minute || cfg.DBPoolMaxIdleTime != 10*time.Minute {
		t.Fatalf("pool lifetimes = %s/%s", cfg.DBPoolMaxLifetime, cfg.DBPoolMaxIdleTime)
	}
	if cfg.DBPoolAcquireTimeout != 1500*time.Millisecond {
		t.Fatalf("DBPoolAcquireTimeout = %s", cfg.DBPoolAcquireTimeout)
	}
	if cfg.DBBackgroundOperationTimeout != 90*time.Second {
		t.Fatalf("DBBackgroundOperationTimeout = %s", cfg.DBBackgroundOperationTimeout)
	}
	if cfg.DBBackgroundMaxConcurrency != 3 {
		t.Fatalf("DBBackgroundMaxConcurrency = %d", cfg.DBBackgroundMaxConcurrency)
	}
}

func TestLoadRejectsInvalidDatabasePoolSettings(t *testing.T) {
	tests := map[string]map[string]string{
		"max below one":       {"DB_POOL_MAX_CONNS": "0"},
		"negative min":        {"DB_POOL_MIN_CONNS": "-1"},
		"min exceeds max":     {"DB_POOL_MAX_CONNS": "2", "DB_POOL_MIN_CONNS": "3"},
		"invalid lifetime":    {"DB_POOL_MAX_CONN_LIFETIME": "forever"},
		"invalid idle time":   {"DB_POOL_MAX_CONN_IDLE_TIME": "0s"},
		"invalid acquire":     {"DB_POOL_ACQUIRE_TIMEOUT": "-1s"},
		"invalid concurrency": {"DB_BACKGROUND_MAX_CONCURRENCY": "0"},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			setConfigEnv(t, values)
			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

func TestLoadAcceptsChineseLocale(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_DEFAULT_LOCALE": "zh-CN",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.DefaultLocale != "zh-CN" {
		t.Fatalf("DefaultLocale = %q, want %q", cfg.DefaultLocale, "zh-CN")
	}
}

func TestLoadRejectsInvalidLocale(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_DEFAULT_LOCALE": "fr-FR",
	})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadAcceptsTimeoutSeconds(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_SHUTDOWN_TIMEOUT_SECONDS": "3",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ShutdownTimeout != 3*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, 3*time.Second)
	}
}

func TestLoadRejectsMalformedTimeout(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_SHUTDOWN_TIMEOUT_SECONDS": "soon",
	})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsNonPositiveTimeout(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			setConfigEnv(t, map[string]string{
				"IDB_SHUTDOWN_TIMEOUT_SECONDS": value,
			})

			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

func TestLoadAcceptsOIDCSettings(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_OIDC_ISSUER":              "https://idb.example.test",
		"IDB_OIDC_KEY_ID":              "key-2026",
		"IDB_OIDC_PRIVATE_KEY_PEM":     "test-private-key",
		"IDB_ACCESS_TOKEN_TTL_SECONDS": "60",
		"IDB_ID_TOKEN_TTL_SECONDS":     "120",
		"IDB_AUTH_CODE_TTL_SECONDS":    "30",
		"IDB_SESSION_TTL_SECONDS":      "3600",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.OIDCIssuer != "https://idb.example.test" {
		t.Fatalf("OIDCIssuer = %q, want %q", cfg.OIDCIssuer, "https://idb.example.test")
	}
	if cfg.OIDCKeyID != "key-2026" {
		t.Fatalf("OIDCKeyID = %q, want %q", cfg.OIDCKeyID, "key-2026")
	}
	if cfg.OIDCPrivateKeyPEM != "test-private-key" {
		t.Fatalf("OIDCPrivateKeyPEM = %q, want configured key", cfg.OIDCPrivateKeyPEM)
	}
	if cfg.AccessTokenTTL != time.Minute {
		t.Fatalf("AccessTokenTTL = %s, want %s", cfg.AccessTokenTTL, time.Minute)
	}
	if cfg.IDTokenTTL != 2*time.Minute {
		t.Fatalf("IDTokenTTL = %s, want %s", cfg.IDTokenTTL, 2*time.Minute)
	}
	if cfg.AuthCodeTTL != 30*time.Second {
		t.Fatalf("AuthCodeTTL = %s, want %s", cfg.AuthCodeTTL, 30*time.Second)
	}
	if cfg.SessionTTL != time.Hour {
		t.Fatalf("SessionTTL = %s, want %s", cfg.SessionTTL, time.Hour)
	}
}

func TestLoadRejectsInvalidOIDCIssuer(t *testing.T) {
	for _, value := range []string{"localhost:8080", "/issuer", "://bad"} {
		t.Run(value, func(t *testing.T) {
			setConfigEnv(t, map[string]string{
				"IDB_OIDC_ISSUER": value,
			})

			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

func TestLoadAcceptsWebBaseURL(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_WEB_BASE_URL": "http://localhost:5180",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.WebBaseURL != "http://localhost:5180" {
		t.Fatalf("WebBaseURL = %q, want http://localhost:5180", cfg.WebBaseURL)
	}
	if cfg.FeishuRedirectURI != "http://localhost:5180/api/auth/feishu/callback" {
		t.Fatalf("FeishuRedirectURI = %q, want frontend callback", cfg.FeishuRedirectURI)
	}
}

func TestLoadAcceptsExplicitFeishuRedirectURI(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_WEB_BASE_URL":        "http://localhost:5180",
		"IDB_FEISHU_REDIRECT_URI": "https://idb.example.test/api/auth/feishu/callback",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.FeishuRedirectURI != "https://idb.example.test/api/auth/feishu/callback" {
		t.Fatalf("FeishuRedirectURI = %q, want explicit value", cfg.FeishuRedirectURI)
	}
}

func TestLoadAcceptsRedisURL(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_REDIS_URL": "redis://localhost:6379/0",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !cfg.RedisEnabled {
		t.Fatal("RedisEnabled = false, want true")
	}
	if cfg.RedisURL != "redis://localhost:6379/0" {
		t.Fatalf("RedisURL = %q", cfg.RedisURL)
	}
}

func TestLoadRejectsRedisEnabledWithoutURL(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_REDIS_ENABLED": "true",
	})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidRedisEnabled(t *testing.T) {
	setConfigEnv(t, map[string]string{
		"IDB_REDIS_ENABLED": "sometimes",
	})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidWebBaseURL(t *testing.T) {
	for _, value := range []string{"localhost:5180", "/dashboard", "://bad"} {
		t.Run(value, func(t *testing.T) {
			setConfigEnv(t, map[string]string{
				"IDB_WEB_BASE_URL": value,
			})

			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

func TestLoadRejectsInvalidOIDCTTL(t *testing.T) {
	for _, key := range []string{
		"IDB_ACCESS_TOKEN_TTL_SECONDS",
		"IDB_ID_TOKEN_TTL_SECONDS",
		"IDB_AUTH_CODE_TTL_SECONDS",
		"IDB_SESSION_TTL_SECONDS",
	} {
		t.Run(key+"_malformed", func(t *testing.T) {
			setConfigEnv(t, map[string]string{key: "soon"})

			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})

		t.Run(key+"_non_positive", func(t *testing.T) {
			setConfigEnv(t, map[string]string{key: "0"})

			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

func TestLoadAcceptsConfigEncryptionKey(t *testing.T) {
	const key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	setConfigEnv(t, map[string]string{"IDB_CONFIG_ENCRYPTION_KEY": key})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ConfigEncryptionKey != key {
		t.Fatalf("ConfigEncryptionKey = %q, want configured key", cfg.ConfigEncryptionKey)
	}
}

func TestLoadRejectsInvalidConfigEncryptionKey(t *testing.T) {
	setConfigEnv(t, map[string]string{"IDB_CONFIG_ENCRYPTION_KEY": "not-a-valid-key"})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid config encryption key error")
	}
}

func TestLoadAcceptsTrustedProxyCIDRs(t *testing.T) {
	setConfigEnv(t, map[string]string{"IDB_TRUSTED_PROXY_CIDRS": "10.0.0.0/8, 2001:db8::/32"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 || cfg.TrustedProxyCIDRs[0].String() != "10.0.0.0/8" || cfg.TrustedProxyCIDRs[1].String() != "2001:db8::/32" {
		t.Fatalf("TrustedProxyCIDRs = %#v", cfg.TrustedProxyCIDRs)
	}
}

func TestLoadRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	setConfigEnv(t, map[string]string{"IDB_TRUSTED_PROXY_CIDRS": "10.0.0.0/8,not-a-cidr"})

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid trusted proxy CIDR error")
	}
}

func setConfigEnv(t *testing.T, values map[string]string) {
	t.Helper()

	for _, key := range []string{
		"IDB_HTTP_ADDR",
		"DATABASE_URL",
		"IDB_DEFAULT_LOCALE",
		"IDB_SHUTDOWN_TIMEOUT_SECONDS",
		"IDB_OIDC_ISSUER",
		"IDB_OIDC_KEY_ID",
		"IDB_OIDC_PRIVATE_KEY_PEM",
		"IDB_CONFIG_ENCRYPTION_KEY",
		"IDB_TRUSTED_PROXY_CIDRS",
		"IDB_WEB_BASE_URL",
		"IDB_FEISHU_REDIRECT_URI",
		"IDB_REDIS_ENABLED",
		"IDB_REDIS_URL",
		"IDB_ACCESS_TOKEN_TTL_SECONDS",
		"IDB_ID_TOKEN_TTL_SECONDS",
		"IDB_AUTH_CODE_TTL_SECONDS",
		"IDB_SESSION_TTL_SECONDS",
		"DB_POOL_MAX_CONNS",
		"DB_POOL_MIN_CONNS",
		"DB_POOL_MAX_CONN_LIFETIME",
		"DB_POOL_MAX_CONN_IDLE_TIME",
		"DB_POOL_ACQUIRE_TIMEOUT",
		"DB_BACKGROUND_OPERATION_TIMEOUT",
		"DB_BACKGROUND_MAX_CONCURRENCY",
	} {
		t.Setenv(key, "")
	}

	for key, value := range values {
		t.Setenv(key, value)
	}
}
