// Unit tests for the sync/async method dispatch read from a
// VolumeReplicationClass (design-sync-replication-csi-addons.md §5, test plan
// U-01..U-04). Pure driver logic: no cluster and no backend. The site is no
// longer a class parameter (design §5.1): it rides on the VolumeReplication's
// SyncSiteAnnotation, so resolveMethod resolves only the method.
package controller

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolveMethod(t *testing.T) {
	cases := []struct {
		name       string
		params     map[string]string
		wantMethod string
		wantCode   codes.Code // OK when no error expected
	}{
		{ // U-01
			name:       "absent method is async",
			params:     map[string]string{},
			wantMethod: methodAsync,
			wantCode:   codes.OK,
		},
		{ // U-02
			name:       "explicit async",
			params:     map[string]string{methodParam: "async"},
			wantMethod: methodAsync,
			wantCode:   codes.OK,
		},
		{ // U-03
			name:       "explicit sync",
			params:     map[string]string{methodParam: "sync"},
			wantMethod: methodSync,
			wantCode:   codes.OK,
		},
		{ // U-03: a stray site parameter is ignored, not read as the method's site
			name:       "sync ignores a site parameter",
			params:     map[string]string{methodParam: "sync", siteParam: "site-a"},
			wantMethod: methodSync,
			wantCode:   codes.OK,
		},
		{ // U-04
			name:     "unknown method is rejected, not treated as async",
			params:   map[string]string{methodParam: "metro"},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, err := resolveMethod(tc.params)
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v (err=%v)", got, tc.wantCode, err)
			}
			if tc.wantCode != codes.OK {
				return // an error case: method is not meaningful
			}
			if method != tc.wantMethod {
				t.Errorf("method = %q, want %q", method, tc.wantMethod)
			}
		})
	}
}
