// SPDX-License-Identifier: MIT

package bootstrap

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfigRequiresExplicitSandboxPolicy(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("IDBRIDGE_SANDBOX_SINGLE_DEVELOPER", "")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "explicitly true or false") {
		t.Fatalf("LoadConfig() error = %v", err)
	}
}

func TestLoadConfigNeverIncludesSecretsInValidationErrors(t *testing.T) {
	setValidEnvironment(t)
	password := "sensitive-admin-password-123"
	clientSecret := "sensitive-directory-client-secret-1234567890"
	t.Setenv("IDBRIDGE_ADMIN_PASSWORD", password)
	t.Setenv("IDBRIDGE_DIRECTORY_READER_CLIENT_SECRET", clientSecret+" ")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() error = nil")
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), clientSecret) {
		t.Fatalf("validation error leaked a secret: %v", err)
	}
}

func TestExpectedPAMRolesDependOnlyOnExplicitPolicy(t *testing.T) {
	tests := []struct {
		name            string
		owner           bool
		singleDeveloper bool
		want            map[string]bool
	}{
		{name: "single developer owner", owner: true, singleDeveloper: true, want: map[string]bool{PAMRequesterRoleCode: true, PAMApproverRoleCode: true}},
		{name: "separated duties owner", owner: true, want: map[string]bool{PAMRequesterRoleCode: true}},
		{name: "unrelated fixture", singleDeveloper: true, want: map[string]bool{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := expectedPAMRoles(test.owner, test.singleDeveloper); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("expectedPAMRoles() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestValidateFixtureProfileTreatsTROBSAsOpaqueMetadata(t *testing.T) {
	spec := fixtureSpec{Username: "owner", Owner: true, TROBSUserID: "opaque-136"}
	if err := validateFixtureProfile([]byte(`{"external_references":{"trobs_user_id":"opaque-136"}}`), spec); err != nil {
		t.Fatalf("validateFixtureProfile() error = %v", err)
	}
	if err := validateFixtureProfile([]byte(`{"external_references":{"trobs_user_id":"opaque-136"}}`), fixtureSpec{Username: "unrelated"}); err == nil {
		t.Fatal("unrelated fixture with TROBS reference was accepted")
	}
}

func setValidEnvironment(t *testing.T) {
	t.Helper()
	values := map[string]string{
		"DATABASE_URL":                               "postgres://idbridge@postgres/idbridge?sslmode=disable",
		"IDB_DEFAULT_LOCALE":                         "en-US",
		"IDBRIDGE_ENTITY_SLUG":                       "isa-sandbox",
		"IDBRIDGE_ENTITY_NAME":                       "ISA Sandbox",
		"IDBRIDGE_ADMIN_USERNAME":                    "sandbox-admin",
		"IDBRIDGE_ADMIN_PASSWORD":                    "sandbox-password-123",
		"IDBRIDGE_ADMIN_ROLE":                        "enterprise_admin",
		"IDBRIDGE_DIRECTORY_READER_APPLICATION_NAME": "ISA IAM Directory Reader",
		"IDBRIDGE_DIRECTORY_READER_CLIENT_ID":        "isa-iam-directory-reader",
		"IDBRIDGE_DIRECTORY_READER_CLIENT_SECRET":    "directory-reader-secret-1234567890",
		"IDBRIDGE_FIXTURE_SOURCE_NAME":               "ISA Sandbox Fixtures",
		"IDBRIDGE_FIXTURE_OWNER_USERNAME":            "fixture-owner",
		"IDBRIDGE_FIXTURE_OWNER_TROBS_USER_ID":       "opaque-136",
		"IDBRIDGE_FIXTURE_UNRELATED_USERNAME":        "fixture-unrelated",
		"IDBRIDGE_SANDBOX_SINGLE_DEVELOPER":          "true",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
}
