// SPDX-License-Identifier: MIT

package adminapi

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/smices/open-idb/internal/db/generated"
)

func TestDirectoryUserTreeNodeProjectsOnlyValidOpaqueExternalReferences(t *testing.T) {
	node := directoryUserTreeNode(generated.DirectoryUser{
		ID: "directory-user-1",
		RawProfile: []byte(`{
  "external_references": {
    "trobs_user_id": "136",
    "worker_ref": "worker-42",
    " bad-key": "ignored",
    "blank": " ",
    "nested": {"value":"ignored"}
  }
}`),
	}, "managed-user-1")
	if node.ID != "managed-user-1" {
		t.Fatalf("ID = %q, want OIDC subject", node.ID)
	}

	if want := map[string]string{"trobs_user_id": "136", "worker_ref": "worker-42"}; !reflect.DeepEqual(node.ExternalReferences, want) {
		t.Fatalf("ExternalReferences = %#v, want %#v", node.ExternalReferences, want)
	}
	if node.Roles != nil {
		t.Fatalf("Roles = %#v, want nil before authoritative IdBridge assignments are loaded", node.Roles)
	}
}

func TestOrganizationTreeRoleCodesAreAuthoritativeAndDeterministic(t *testing.T) {
	roles := organizationTreeRoleCodes([]generated.Role{
		{Code: "isa:pam:requester"},
		{Code: "employee"},
		{Code: "isa:pam:approver"},
	})
	want := []string{"employee", "isa:pam:approver", "isa:pam:requester"}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles = %#v, want %#v", roles, want)
	}
}

func TestDirectoryUserTreeNodeDoesNotProjectMalformedOrUnboundedReferences(t *testing.T) {
	tooLongKey := strings.Repeat("k", maxExternalReferenceKeyLength+1)
	tooLongValue := strings.Repeat("v", maxExternalReferenceValueLength+1)
	node := directoryUserTreeNode(generated.DirectoryUser{
		RawProfile: []byte(`{"external_references":{"` + tooLongKey + `":"one","too_long":"` + tooLongValue + `"}}`),
	}, "managed-user-1")
	if node.ExternalReferences != nil {
		t.Fatalf("ExternalReferences = %#v, want nil", node.ExternalReferences)
	}

	node = directoryUserTreeNode(generated.DirectoryUser{RawProfile: []byte(`{"external_references":[]}`)}, "managed-user-1")
	if node.ExternalReferences != nil {
		t.Fatalf("ExternalReferences = %#v, want nil", node.ExternalReferences)
	}

	var profile strings.Builder
	profile.WriteString(`{"external_references":{`)
	for index := 0; index <= maxExternalReferences; index++ {
		if index > 0 {
			profile.WriteByte(',')
		}
		fmt.Fprintf(&profile, `"key_%d":"value_%d"`, index, index)
	}
	profile.WriteString(`}}`)
	node = directoryUserTreeNode(generated.DirectoryUser{RawProfile: []byte(profile.String())}, "managed-user-1")
	if node.ExternalReferences != nil {
		t.Fatalf("ExternalReferences = %#v, want nil for over-limit map", node.ExternalReferences)
	}
}
