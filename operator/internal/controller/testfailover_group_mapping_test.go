package controller

import (
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	mapSnapWeb = "snap-web"
	mapSnapDB  = "snap-db"
	mapTgtWeb  = "tgt-web"
	mapTgtDB   = "tgt-db"
	mapSrcWeb  = "src-web"
	mapSrcDB   = "src-db"
)

// The two members of a web+db group, as the drill resolved them.
var mappingSlots = []groupMemberSource{
	{PVC: "web-disk", LvolID: mapSrcWeb},
	{PVC: "db-disk", LvolID: mapSrcDB},
}

// TestAssignGenerationMembersBySourceShuffled covers the regression: the control
// plane lists the generation's members in its own order (db first here), and each
// PVC must still get its own member's snapshot, not the one at its position.
func TestAssignGenerationMembersBySourceShuffled(t *testing.T) {
	members := []webapi.ReplicatedGroupSnapshot{
		{SnapshotID: mapSnapDB, LvolID: mapTgtDB, SourceLvolID: mapSrcDB},
		{SnapshotID: mapSnapWeb, LvolID: mapTgtWeb, SourceLvolID: mapSrcWeb},
	}
	got, err := assignGenerationMembers(mappingSlots, members, nil)
	if err != nil {
		t.Fatal(err)
	}
	if members[got[0]].SnapshotID != mapSnapWeb || members[got[1]].SnapshotID != mapSnapDB {
		t.Errorf("web-disk got %s, db-disk got %s; want snap-web, snap-db",
			members[got[0]].SnapshotID, members[got[1]].SnapshotID)
	}
}

// TestAssignGenerationMembersByReplicaVolume covers a control plane that names
// only the replica volume on the target: the pairing goes through the source to
// replica map and is still order-independent.
func TestAssignGenerationMembersByReplicaVolume(t *testing.T) {
	members := []webapi.ReplicatedGroupSnapshot{
		{SnapshotID: mapSnapDB, LvolID: mapTgtDB},
		{SnapshotID: mapSnapWeb, LvolID: mapTgtWeb},
	}
	targetOf := map[string]string{mapSrcWeb: mapTgtWeb, mapSrcDB: mapTgtDB}
	got, err := assignGenerationMembers(mappingSlots, members, targetOf)
	if err != nil {
		t.Fatal(err)
	}
	if members[got[0]].SnapshotID != mapSnapWeb || members[got[1]].SnapshotID != mapSnapDB {
		t.Errorf("web-disk got %s, db-disk got %s; want snap-web, snap-db",
			members[got[0]].SnapshotID, members[got[1]].SnapshotID)
	}
}

// TestAssignGenerationMembersRefusesMismatch covers every way the pairing can be
// incomplete: none of them falls back to position.
func TestAssignGenerationMembersRefusesMismatch(t *testing.T) {
	cases := []struct {
		name     string
		members  []webapi.ReplicatedGroupSnapshot
		targetOf map[string]string
		want     string
	}{
		{
			name: "missing member",
			members: []webapi.ReplicatedGroupSnapshot{
				{SnapshotID: mapSnapWeb, LvolID: mapTgtWeb, SourceLvolID: mapSrcWeb},
				{SnapshotID: "snap-other", LvolID: "tgt-other", SourceLvolID: "src-other"},
			},
			want: "no snapshot for db-disk",
		},
		{
			name: "stray member",
			members: []webapi.ReplicatedGroupSnapshot{
				{SnapshotID: mapSnapWeb, LvolID: mapTgtWeb, SourceLvolID: mapSrcWeb},
				{SnapshotID: mapSnapDB, LvolID: mapTgtDB, SourceLvolID: mapSrcDB},
				{SnapshotID: "snap-x", LvolID: "tgt-x", SourceLvolID: "src-x"},
			},
			want: "snap-x (volume src-x) match no PVC",
		},
		{
			name: "duplicate member",
			members: []webapi.ReplicatedGroupSnapshot{
				{SnapshotID: "snap-a", LvolID: mapTgtWeb, SourceLvolID: mapSrcWeb},
				{SnapshotID: "snap-b", LvolID: mapTgtWeb, SourceLvolID: mapSrcWeb},
			},
			want: "both belong to volume src-web",
		},
		{
			name: "no replica volume known",
			members: []webapi.ReplicatedGroupSnapshot{
				{SnapshotID: mapSnapWeb, LvolID: mapTgtWeb},
				{SnapshotID: mapSnapDB, LvolID: mapTgtDB},
			},
			targetOf: map[string]string{mapSrcWeb: mapTgtWeb},
			want:     "db-disk (source volume src-db has no replica volume",
		},
		{
			name: "member without a volume",
			members: []webapi.ReplicatedGroupSnapshot{
				{SnapshotID: mapSnapWeb, LvolID: mapTgtWeb},
				{SnapshotID: mapSnapDB},
			},
			targetOf: map[string]string{mapSrcWeb: mapTgtWeb, mapSrcDB: mapTgtDB},
			want:     "snap-db names no volume",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := assignGenerationMembers(mappingSlots, tc.members, tc.targetOf)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
