package main

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

type fakeServerGroupsGetter struct {
	groups []string
}

func (f fakeServerGroupsGetter) ServerGroups() (*metav1.APIGroupList, error) {
	list := &metav1.APIGroupList{Groups: make([]metav1.APIGroup, 0, len(f.groups))}
	for _, group := range f.groups {
		list.Groups = append(list.Groups, metav1.APIGroup{Name: group})
	}
	return list, nil
}

type fakeServerResourcesGetter struct {
	// groupVersion -> resource names served. A missing key means the server does
	// not serve that group/version at all (discovery returns NotFound).
	resources map[string][]string
}

func (f fakeServerResourcesGetter) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	names, ok := f.resources[gv]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: gv}, "")
	}
	list := &metav1.APIResourceList{GroupVersion: gv}
	for _, n := range names {
		list.APIResources = append(list.APIResources, metav1.APIResource{Name: n})
	}
	return list, nil
}

// Regression: 2026-10-01 — the operator crashed at startup on every cluster
// outside the hub. The TestFailover controller unconditionally watched OCM's
// ManifestWork (.Owns(&workv1.ManifestWork{})), but ManifestWork is served only
// on the hub — managed clusters serve the same group/version for
// AppliedManifestWork (the work-agent's local record) yet never serve
// ManifestWork. The controller's cache never syncs and the manager exits
// ("failed to wait for testfailover caches to sync ... *v1.ManifestWork"),
// taking every other controller (replication, storagecluster, …) down with it.
// Registration must be gated on the ManifestWork RESOURCE being served, not the
// work API group — a group-level check sees the group as served wherever
// AppliedManifestWork exists and so does not skip the controller (live 2026-10-01).
func TestServerHasManifestWork(t *testing.T) {
	tests := []struct {
		name      string
		resources map[string][]string
		want      bool
	}{
		{
			name:      "hub serves ManifestWork",
			resources: map[string][]string{ocmWorkGroupVersion: {"manifestworks", "appliedmanifestworks"}},
			want:      true,
		},
		{
			name:      "managed cluster serves only AppliedManifestWork",
			resources: map[string][]string{ocmWorkGroupVersion: {"appliedmanifestworks"}},
			want:      false,
		},
		{
			name:      "cluster without OCM at all",
			resources: map[string][]string{},
			want:      false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serverHasResource(
				fakeServerResourcesGetter{resources: tc.resources}, ocmWorkGroupVersion, ocmManifestWorkResource)
			if err != nil {
				t.Fatalf("serverHasResource returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("serverHasResource = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateTLSConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		tlsEnabled  bool
		tlsProvider string
		groups      []string
		wantErr     string
	}{
		{
			name:        "tls disabled skips validation",
			tlsEnabled:  false,
			tlsProvider: utils.TLSProviderOpenShift,
		},
		{
			name:        "openshift provider accepts openshift api group",
			tlsEnabled:  true,
			tlsProvider: utils.TLSProviderOpenShift,
			groups:      []string{openShiftConfigAPIGroup},
		},
		{
			name:        "cert-manager provider accepts cert-manager api group",
			tlsEnabled:  true,
			tlsProvider: utils.TLSProviderCertManager,
			groups:      []string{certManagerAPIGroup},
		},
		{
			name:        "unsupported provider rejected",
			tlsEnabled:  true,
			tlsProvider: "vault",
			wantErr:     `unsupported SB_TLS_PROVIDER "vault"`,
		},
		{
			name:        "openshift provider requires openshift api group",
			tlsEnabled:  true,
			tlsProvider: utils.TLSProviderOpenShift,
			groups:      []string{certManagerAPIGroup},
			wantErr:     openShiftConfigAPIGroup,
		},
		{
			name:        "cert-manager provider requires cert-manager api group",
			tlsEnabled:  true,
			tlsProvider: utils.TLSProviderCertManager,
			groups:      []string{openShiftConfigAPIGroup},
			wantErr:     certManagerAPIGroup,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTLSConfiguration(fakeServerGroupsGetter{groups: tc.groups}, tc.tlsEnabled, tc.tlsProvider)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTLSConfiguration returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
