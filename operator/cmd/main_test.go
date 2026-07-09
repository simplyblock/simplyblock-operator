package main

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
