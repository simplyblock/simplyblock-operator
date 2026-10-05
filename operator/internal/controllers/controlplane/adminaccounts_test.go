package controlplane

import "testing"

// The Control Center reads every storage cluster through the management API
// with its own service account; the chart names it in the operator's
// environment and the management API trusts it next to the operator.
func TestExtraAdminServiceAccountsAreAppendedOnceAndValidated(t *testing.T) {
	t.Setenv(extraAdminAccountsEnv,
		" system:serviceaccount:simplyblock:console , not-an-account,system:serviceaccount:a:b:c,"+
			"system:serviceaccount:simplyblock:console")
	want := "system:serviceaccount:sb:simplyblock-operator,system:serviceaccount:simplyblock:console"
	if got := adminServiceAccounts("sb"); got != want {
		t.Errorf("adminServiceAccounts = %q, want %q", got, want)
	}
}

// Without the operator's variable the management API trusts the operator alone.
func TestNoExtraAdminServiceAccountsMeansTheOperatorAlone(t *testing.T) {
	t.Setenv(extraAdminAccountsEnv, "")
	if got, want := adminServiceAccounts("sb"), "system:serviceaccount:sb:simplyblock-operator"; got != want {
		t.Errorf("adminServiceAccounts = %q, want %q", got, want)
	}
}
