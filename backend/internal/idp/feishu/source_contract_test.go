// SPDX-License-Identifier: MIT

package feishu

import (
	"os"
	"strings"
	"testing"
)

func TestVersionedSourceContractCoversDestructiveAndAuthorizationBoundaries(t *testing.T) {
	content, err := os.ReadFile("../../../../docs/contracts/feishu-trobs-source-contract-v1.md")
	if err != nil {
		t.Fatalf("read source contract: %v", err)
	}
	text := string(content)
	required := []string{
		"idbridge-source-feishu-trobs-v1",
		"contact/v3/departments/0/children",
		"contact/v3/users/find_by_department",
		"A→B→A",
		"1,000 pages",
		"zero-row user or department snapshot",
		"at least 50 percent",
		"fewer than 20 current rows",
		"top-level opaque `version`",
		"confirmed incremental delete",
		"credentials are never",
		"TROBS responsibilities",
		"must not be ingested as IdBridge roles",
		"isa:pam:requester",
		"isa:pam:approver",
	}
	for _, phrase := range required {
		if !strings.Contains(text, phrase) {
			t.Errorf("source contract is missing %q", phrase)
		}
	}
}
