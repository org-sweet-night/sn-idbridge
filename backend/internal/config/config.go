// SPDX-License-Identifier: MIT

package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/smices/open-idb/internal/secureconfig"
)

type Config struct {
	HTTPAddr                     string
	DatabaseURL                  string
	DBPoolMaxConns               int32
	DBPoolMinConns               int32
	DBPoolMinConnsSet            bool
	DBPoolMaxLifetime            time.Duration
	DBPoolMaxIdleTime            time.Duration
	DBPoolAcquireTimeout         time.Duration
	DBBackgroundOperationTimeout time.Duration
	DBBackgroundMaxConcurrency   int
	DefaultLocale                string
	ShutdownTimeout              time.Duration
	OIDCIssuer                   string
	OIDCKeyID                    string
	OIDCPrivateKeyPEM            string
	ConfigEncryptionKey          string
	AccessTokenTTL               time.Duration
	IDTokenTTL                   time.Duration
	AuthCodeTTL                  time.Duration
	SessionTTL                   time.Duration
	RedisEnabled                 bool
	RedisURL                     string
	WebBaseURL                   string
	FeishuAppID                  string
	FeishuAppSecret              string
	FeishuBaseURL                string
	FeishuRedirectURI            string
	TrustedProxyCIDRs            []netip.Prefix
}

func Load() (Config, error) {
	shutdownTimeout, err := getDurationSeconds("IDB_SHUTDOWN_TIMEOUT_SECONDS", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbPoolMaxConns, err := getInt32("DB_POOL_MAX_CONNS", 0, 1)
	if err != nil {
		return Config{}, err
	}
	dbPoolMinConns, err := getInt32("DB_POOL_MIN_CONNS", 0, 0)
	if err != nil {
		return Config{}, err
	}
	dbPoolMaxLifetime, err := getOptionalDuration("DB_POOL_MAX_CONN_LIFETIME")
	if err != nil {
		return Config{}, err
	}
	dbPoolMaxIdleTime, err := getOptionalDuration("DB_POOL_MAX_CONN_IDLE_TIME")
	if err != nil {
		return Config{}, err
	}
	dbPoolAcquireTimeout, err := getDuration("DB_POOL_ACQUIRE_TIMEOUT", 2*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbBackgroundOperationTimeout, err := getDuration("DB_BACKGROUND_OPERATION_TIMEOUT", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	dbBackgroundMaxConcurrency, err := getInt("DB_BACKGROUND_MAX_CONCURRENCY", 2, 1)
	if err != nil {
		return Config{}, err
	}
	accessTokenTTL, err := getDurationSeconds("IDB_ACCESS_TOKEN_TTL_SECONDS", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	idTokenTTL, err := getDurationSeconds("IDB_ID_TOKEN_TTL_SECONDS", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	authCodeTTL, err := getDurationSeconds("IDB_AUTH_CODE_TTL_SECONDS", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	sessionTTL, err := getDurationSeconds("IDB_SESSION_TTL_SECONDS", 24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	redisEnabled, err := getBool("IDB_REDIS_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	trustedProxyCIDRs, err := getCIDRs("IDB_TRUSTED_PROXY_CIDRS")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		HTTPAddr:                     getEnv("IDB_HTTP_ADDR", ":8080"),
		DatabaseURL:                  os.Getenv("DATABASE_URL"),
		DBPoolMaxConns:               dbPoolMaxConns,
		DBPoolMinConns:               dbPoolMinConns,
		DBPoolMinConnsSet:            strings.TrimSpace(os.Getenv("DB_POOL_MIN_CONNS")) != "",
		DBPoolMaxLifetime:            dbPoolMaxLifetime,
		DBPoolMaxIdleTime:            dbPoolMaxIdleTime,
		DBPoolAcquireTimeout:         dbPoolAcquireTimeout,
		DBBackgroundOperationTimeout: dbBackgroundOperationTimeout,
		DBBackgroundMaxConcurrency:   dbBackgroundMaxConcurrency,
		DefaultLocale:                getEnv("IDB_DEFAULT_LOCALE", "en-US"),
		ShutdownTimeout:              shutdownTimeout,
		OIDCIssuer:                   getEnv("IDB_OIDC_ISSUER", "http://localhost:8080"),
		OIDCKeyID:                    getEnv("IDB_OIDC_KEY_ID", "dev-key-1"),
		OIDCPrivateKeyPEM:            os.Getenv("IDB_OIDC_PRIVATE_KEY_PEM"),
		ConfigEncryptionKey:          strings.TrimSpace(os.Getenv("IDB_CONFIG_ENCRYPTION_KEY")),
		AccessTokenTTL:               accessTokenTTL,
		IDTokenTTL:                   idTokenTTL,
		AuthCodeTTL:                  authCodeTTL,
		SessionTTL:                   sessionTTL,
		RedisEnabled:                 redisEnabled,
		RedisURL:                     os.Getenv("IDB_REDIS_URL"),
		WebBaseURL:                   os.Getenv("IDB_WEB_BASE_URL"),
		FeishuAppID:                  os.Getenv("IDB_FEISHU_APP_ID"),
		FeishuAppSecret:              os.Getenv("IDB_FEISHU_APP_SECRET"),
		FeishuBaseURL:                getEnv("IDB_FEISHU_BASE_URL", "https://open.feishu.cn"),
		FeishuRedirectURI:            getEnv("IDB_FEISHU_REDIRECT_URI", "http://localhost:8080/api/auth/feishu/callback"),
		TrustedProxyCIDRs:            trustedProxyCIDRs,
	}

	if cfg.DefaultLocale != "en-US" && cfg.DefaultLocale != "zh-CN" {
		return Config{}, fmt.Errorf("IDB_DEFAULT_LOCALE must be en-US or zh-CN")
	}
	if err := validateAbsoluteURL("IDB_OIDC_ISSUER", cfg.OIDCIssuer); err != nil {
		return Config{}, err
	}
	if cfg.WebBaseURL != "" {
		if err := validateAbsoluteURL("IDB_WEB_BASE_URL", cfg.WebBaseURL); err != nil {
			return Config{}, err
		}
		if os.Getenv("IDB_FEISHU_REDIRECT_URI") == "" {
			cfg.FeishuRedirectURI = strings.TrimRight(cfg.WebBaseURL, "/") + "/api/auth/feishu/callback"
		}
	}
	if err := validateAbsoluteURL("IDB_FEISHU_REDIRECT_URI", cfg.FeishuRedirectURI); err != nil {
		return Config{}, err
	}
	if cfg.RedisURL != "" {
		cfg.RedisEnabled = true
	}
	if cfg.RedisEnabled && cfg.RedisURL == "" {
		return Config{}, fmt.Errorf("IDB_REDIS_URL is required when IDB_REDIS_ENABLED is true")
	}
	if cfg.DBPoolMaxConns > 0 && cfg.DBPoolMinConns > cfg.DBPoolMaxConns {
		return Config{}, fmt.Errorf("DB_POOL_MIN_CONNS must not exceed DB_POOL_MAX_CONNS")
	}
	if _, err := secureconfig.New(cfg.ConfigEncryptionKey); err != nil {
		return Config{}, fmt.Errorf("IDB_CONFIG_ENCRYPTION_KEY: %w", err)
	}

	return cfg, nil
}

func getInt32(key string, fallback int32, minimum int32) (int32, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < int64(minimum) {
		return 0, fmt.Errorf("%s must be an integer greater than or equal to %d", key, minimum)
	}
	return int32(parsed), nil
}

func getInt(key string, fallback int, minimum int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum {
		return 0, fmt.Errorf("%s must be an integer greater than or equal to %d", key, minimum)
	}
	return parsed, nil
}

func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}

func getOptionalDuration(key string) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, nil
	}
	return getDuration(key, 0)
}

func getCIDRs(key string) ([]netip.Prefix, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil, nil
	}
	prefixes := make([]netip.Prefix, 0, strings.Count(value, ",")+1)
	for _, item := range strings.Split(value, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil {
			return nil, fmt.Errorf("%s contains invalid CIDR %q: %w", key, strings.TrimSpace(item), err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func getEnv(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func getDurationSeconds(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive integer number of seconds", key)
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	return time.Duration(seconds) * time.Second, nil
}

func getBool(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return parsed, nil
}

func validateAbsoluteURL(key string, value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s must be an absolute URL: %w", key, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute URL with scheme and host", key)
	}
	return nil
}
