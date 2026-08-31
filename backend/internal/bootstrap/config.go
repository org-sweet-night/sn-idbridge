// SPDX-License-Identifier: MIT

// Package bootstrap owns the noninteractive, in-process sandbox bootstrap.
// It intentionally exposes no HTTP surface and never serializes credentials.
package bootstrap

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	EmployeeRoleCode     = "employee"
	PAMRequesterRoleCode = "isa:pam:requester"
	PAMApproverRoleCode  = "isa:pam:approver"
	DirectoryReadScope   = "directory:read"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}[a-z0-9]$`)

type Config struct {
	DatabaseURL                    string
	DefaultLocale                  string
	EntitySlug                     string
	EntityName                     string
	AdminUsername                  string
	AdminPassword                  string
	AdminRole                      string
	DirectoryReaderApplicationName string
	DirectoryReaderClientID        string
	DirectoryReaderClientSecret    string
	FixtureSourceName              string
	FixtureOwnerUsername           string
	FixtureOwnerTROBSUserID        string
	FixtureUnrelatedUsername       string
	SandboxSingleDeveloper         bool
}

// LoadConfig reads only the values needed by `idbridge bootstrap sandbox`.
// Server-only signing and encryption settings are deliberately not required.
func LoadConfig() (Config, error) {
	cfg := Config{
		DatabaseURL:                    strings.TrimSpace(os.Getenv("DATABASE_URL")),
		DefaultLocale:                  strings.TrimSpace(os.Getenv("IDB_DEFAULT_LOCALE")),
		EntitySlug:                     strings.TrimSpace(os.Getenv("IDBRIDGE_ENTITY_SLUG")),
		EntityName:                     strings.TrimSpace(os.Getenv("IDBRIDGE_ENTITY_NAME")),
		AdminUsername:                  strings.TrimSpace(os.Getenv("IDBRIDGE_ADMIN_USERNAME")),
		AdminPassword:                  os.Getenv("IDBRIDGE_ADMIN_PASSWORD"),
		AdminRole:                      strings.TrimSpace(os.Getenv("IDBRIDGE_ADMIN_ROLE")),
		DirectoryReaderApplicationName: strings.TrimSpace(os.Getenv("IDBRIDGE_DIRECTORY_READER_APPLICATION_NAME")),
		DirectoryReaderClientID:        strings.TrimSpace(os.Getenv("IDBRIDGE_DIRECTORY_READER_CLIENT_ID")),
		DirectoryReaderClientSecret:    os.Getenv("IDBRIDGE_DIRECTORY_READER_CLIENT_SECRET"),
		FixtureSourceName:              strings.TrimSpace(os.Getenv("IDBRIDGE_FIXTURE_SOURCE_NAME")),
		FixtureOwnerUsername:           strings.TrimSpace(os.Getenv("IDBRIDGE_FIXTURE_OWNER_USERNAME")),
		FixtureOwnerTROBSUserID:        strings.TrimSpace(os.Getenv("IDBRIDGE_FIXTURE_OWNER_TROBS_USER_ID")),
		FixtureUnrelatedUsername:       strings.TrimSpace(os.Getenv("IDBRIDGE_FIXTURE_UNRELATED_USERNAME")),
	}
	if cfg.DefaultLocale == "" {
		cfg.DefaultLocale = "en-US"
	}
	policy := strings.TrimSpace(os.Getenv("IDBRIDGE_SANDBOX_SINGLE_DEVELOPER"))
	if policy != "true" && policy != "false" {
		return Config{}, fmt.Errorf("IDBRIDGE_SANDBOX_SINGLE_DEVELOPER must be explicitly true or false")
	}
	cfg.SandboxSingleDeveloper = policy == "true"
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	required := map[string]string{
		"DATABASE_URL":                               c.DatabaseURL,
		"IDBRIDGE_ENTITY_SLUG":                       c.EntitySlug,
		"IDBRIDGE_ENTITY_NAME":                       c.EntityName,
		"IDBRIDGE_ADMIN_USERNAME":                    c.AdminUsername,
		"IDBRIDGE_ADMIN_PASSWORD":                    c.AdminPassword,
		"IDBRIDGE_ADMIN_ROLE":                        c.AdminRole,
		"IDBRIDGE_DIRECTORY_READER_APPLICATION_NAME": c.DirectoryReaderApplicationName,
		"IDBRIDGE_DIRECTORY_READER_CLIENT_ID":        c.DirectoryReaderClientID,
		"IDBRIDGE_DIRECTORY_READER_CLIENT_SECRET":    c.DirectoryReaderClientSecret,
		"IDBRIDGE_FIXTURE_SOURCE_NAME":               c.FixtureSourceName,
		"IDBRIDGE_FIXTURE_OWNER_USERNAME":            c.FixtureOwnerUsername,
		"IDBRIDGE_FIXTURE_OWNER_TROBS_USER_ID":       c.FixtureOwnerTROBSUserID,
		"IDBRIDGE_FIXTURE_UNRELATED_USERNAME":        c.FixtureUnrelatedUsername,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if c.DefaultLocale != "en-US" && c.DefaultLocale != "zh-CN" {
		return fmt.Errorf("IDB_DEFAULT_LOCALE must be en-US or zh-CN")
	}
	if !slugPattern.MatchString(c.EntitySlug) {
		return fmt.Errorf("IDBRIDGE_ENTITY_SLUG must be a lowercase DNS-like slug")
	}
	if c.AdminUsername != strings.ToLower(c.AdminUsername) {
		return fmt.Errorf("IDBRIDGE_ADMIN_USERNAME must be lowercase")
	}
	if c.AdminRole != "platform_admin" && c.AdminRole != "enterprise_admin" {
		return fmt.Errorf("IDBRIDGE_ADMIN_ROLE must be platform_admin or enterprise_admin")
	}
	if weakAdminPassword(c.AdminPassword) {
		return fmt.Errorf("IDBRIDGE_ADMIN_PASSWORD must be at least 12 characters and contain a letter and a digit")
	}
	if c.DirectoryReaderClientSecret != strings.TrimSpace(c.DirectoryReaderClientSecret) || len(c.DirectoryReaderClientSecret) < 32 || len(c.DirectoryReaderClientSecret) > 4096 {
		return fmt.Errorf("IDBRIDGE_DIRECTORY_READER_CLIENT_SECRET must be 32 to 4096 non-whitespace-surrounded characters")
	}
	if c.FixtureOwnerUsername == c.FixtureUnrelatedUsername {
		return fmt.Errorf("owner and unrelated fixture usernames must differ")
	}
	return nil
}

func weakAdminPassword(password string) bool {
	if password != strings.TrimSpace(password) || len(password) < 12 || len(password) > 4096 {
		return true
	}
	return !strings.ContainsAny(password, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") ||
		!strings.ContainsAny(password, "0123456789")
}

// expectedPAMRoles is driven only by explicit sandbox policy. TROBS metadata is
// deliberately absent so a business reference can never grant authorization.
func expectedPAMRoles(owner bool, singleDeveloper bool) map[string]bool {
	roles := map[string]bool{}
	if !owner {
		return roles
	}
	roles[PAMRequesterRoleCode] = true
	if singleDeveloper {
		roles[PAMApproverRoleCode] = true
	}
	return roles
}
