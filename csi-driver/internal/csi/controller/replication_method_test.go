// Unit tests for the sync/async method dispatch read from a
// VolumeReplicationClass (design-sync-replication-csi-addons.md §5, test plan
// U-01..U-06). Pure driver logic: no cluster and no backend.
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
		wantSite   string
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
		{ // U-03, U-05
			name:       "sync with site",
			params:     map[string]string{methodParam: "sync", siteParam: "site-a"},
			wantMethod: methodSync,
			wantSite:   "site-a",
			wantCode:   codes.OK,
		},
		{ // U-06
			name:     "sync without site is InvalidArgument",
			params:   map[string]string{methodParam: "sync"},
			wantCode: codes.InvalidArgument,
		},
		{ // U-04
			name:     "unknown method is rejected, not treated as async",
			params:   map[string]string{methodParam: "metro"},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, site, err := resolveMethod(tc.params)
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v (err=%v)", got, tc.wantCode, err)
			}
			if tc.wantCode != codes.OK {
				return // an error case: method/site are not meaningful
			}
			if method != tc.wantMethod {
				t.Errorf("method = %q, want %q", method, tc.wantMethod)
			}
			if site != tc.wantSite {
				t.Errorf("site = %q, want %q", site, tc.wantSite)
			}
		})
	}
}
