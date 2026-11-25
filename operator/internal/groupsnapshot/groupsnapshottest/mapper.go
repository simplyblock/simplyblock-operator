// Package groupsnapshottest builds the REST mapping of a cluster that serves
// the upstream VolumeGroupSnapshot kinds at chosen versions, for fake clients.
//
// A fake client starts with an empty REST mapper, which is a cluster that
// serves no groupsnapshot.storage.k8s.io version at all. A test that means a
// cluster with the kinds says which versions it serves through this mapper.
package groupsnapshottest

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/simplyblock/simplyblock-operator/internal/groupsnapshot"
)

// Mapper is a REST mapper that knows the three VolumeGroupSnapshot kinds at
// each of versions, in the order given, and nothing else.
func Mapper(versions ...string) meta.RESTMapper {
	gvs := make([]schema.GroupVersion, 0, len(versions))
	for _, v := range versions {
		gvs = append(gvs, schema.GroupVersion{Group: groupsnapshot.Group, Version: v})
	}
	m := meta.NewDefaultRESTMapper(gvs)
	for _, gv := range gvs {
		m.Add(gv.WithKind(groupsnapshot.KindVolumeGroupSnapshot), meta.RESTScopeNamespace)
		m.Add(gv.WithKind(groupsnapshot.KindVolumeGroupSnapshotClass), meta.RESTScopeRoot)
		m.Add(gv.WithKind(groupsnapshot.KindVolumeGroupSnapshotContent), meta.RESTScopeRoot)
	}
	return m
}
