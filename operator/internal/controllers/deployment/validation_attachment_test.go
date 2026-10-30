// What validation says about a document whose groups attach their storage
// nodes through NetworkAttachmentDefinitions instead of naming host interfaces.

package deployment

import (
	"slices"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func attachedDocument(groups ...simplyblockv1alpha2.NodeGroup) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return &simplyblockv1alpha2.ClusterDeploymentConfig{Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
		NodeSets: []simplyblockv1alpha2.NodeSet{{Name: "discovered", Groups: groups}},
	}}
}

func attachedGroup(name, management, data string) simplyblockv1alpha2.NodeGroup {
	return simplyblockv1alpha2.NodeGroup{
		Name: name, Workers: []string{name + "-worker"},
		NetworkAttachment: &simplyblockv1alpha2.NetworkAttachmentSelection{Management: management, Data: data},
	}
}

// An attached group names its management attachment, which is what a
// host-networked group's interface is for, so the finding about a group naming
// no management interface does not describe it.
func TestAnAttachedGroupIsNotReportedForNamingNoManagementInterface(t *testing.T) {
	doc := attachedDocument(attachedGroup("g", "storage-mgmt", ""))
	if found := groupsWithoutManagementInterface(doc); len(found) > 0 {
		t.Errorf("the attached groups %v are reported as naming no management interface", found)
	}
}

// The storage nodes of an attached group are not built with the attachment
// yet, so the document is held from approval under a reason that says so.
func TestAnAttachedGroupIsHeldAsNotYetDeployable(t *testing.T) {
	doc := attachedDocument(attachedGroup("g", "storage-mgmt", ""), attachedGroup("h", "storage-mgmt", ""))
	if found := groupsWithNetworkAttachments(doc); !slices.Equal(found, []string{"g", "h"}) {
		t.Errorf("the attached groups are %v, want g and h", found)
	}
}

// One DaemonSet serves every node of a cluster, so its groups cannot attach
// through different definitions any more than they can bind different
// interfaces.
func TestGroupsNamingDifferentAttachmentsConflict(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		groups []simplyblockv1alpha2.NodeGroup
		want   bool
	}{
		{"the same attachments", []simplyblockv1alpha2.NodeGroup{
			attachedGroup("g", "storage-mgmt", "storage-data"), attachedGroup("h", "storage-mgmt", "storage-data")}, false},
		{"different management attachments", []simplyblockv1alpha2.NodeGroup{
			attachedGroup("g", "storage-mgmt", ""), attachedGroup("h", "other-mgmt", "")}, true},
		{"one group with a data attachment", []simplyblockv1alpha2.NodeGroup{
			attachedGroup("g", "storage-mgmt", "storage-data"), attachedGroup("h", "storage-mgmt", "")}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			found := conflictingInterfaces(attachedDocument(testCase.groups...))
			if got := found != ""; got != testCase.want {
				t.Errorf("conflict reported %v (%q), want %v", got, found, testCase.want)
			}
			if testCase.want && !strings.Contains(found, "network attachments") {
				t.Errorf("the conflict %q does not name the attachments", found)
			}
		})
	}
}
