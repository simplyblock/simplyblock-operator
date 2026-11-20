package v1alpha2

import "testing"

func TestGroupBackupLabelValueIsTheBareUUID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"e61641ee-721f-4c6b-a782-675a4ae9e83b/e908ea4d-d025-42e2-aa87-526b6267a37d", "e908ea4d-d025-42e2-aa87-526b6267a37d"},
		{"e908ea4d-d025-42e2-aa87-526b6267a37d", "e908ea4d-d025-42e2-aa87-526b6267a37d"},
		{"", ""},
	} {
		if got := GroupBackupLabelValue(tc.in); got != tc.want {
			t.Errorf("GroupBackupLabelValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
