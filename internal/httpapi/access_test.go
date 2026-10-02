package httpapi

import (
	"context"
	"testing"
)

func TestHasMinRole(t *testing.T) {
	cases := []struct {
		roles []string
		min   string
		want  bool
	}{
		{nil, "support", false},
		{[]string{"support"}, "support", true},
		{[]string{"support"}, "moderator", false},
		{[]string{"moderator"}, "support", true}, // moderator satisfies a support-or-above requirement
		{[]string{"super_admin"}, "moderator", true},
		{[]string{"support", "moderator"}, "super_admin", false},
		// An unranked role (typo, removed role, whatever) must never grant
		// access, even against an equally-unranked/unknown minRole where
		// both ranks would otherwise compare equal at their zero value.
		{[]string{"bogus_role"}, "bogus_role", false},
		{[]string{"bogus_role"}, "support", false},
	}
	for _, c := range cases {
		if got := hasMinRole(c.roles, c.min); got != c.want {
			t.Errorf("hasMinRole(%v, %q) = %v, want %v", c.roles, c.min, got, c.want)
		}
	}
}

// TestRequireAdmin_UnknownMinRolePanics covers the construction-time guard:
// a minRole that isn't in roleRank would otherwise make every request to
// that route silently 403 forever (roleRank[minRole] == 0 satisfies no
// role), which should fail loudly at startup instead.
func TestRequireAdmin_UnknownMinRolePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("requireAdmin(unknown role) should panic")
		}
	}()
	(&Server{}).requireAdmin("not_a_real_role")
}

// TestRequireAdmin_KnownMinRolesDoNotPanic is a sanity check that every
// role actually in roleRank is accepted at construction time.
func TestRequireAdmin_KnownMinRolesDoNotPanic(t *testing.T) {
	for role := range roleRank {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("requireAdmin(%q) panicked: %v", role, r)
				}
			}()
			(&Server{}).requireAdmin(role)
		}()
	}
}

func TestAdminRolesFrom(t *testing.T) {
	if roles := adminRolesFrom(context.Background()); roles != nil {
		t.Errorf("expected nil roles from a bare context, got %v", roles)
	}
	ctx := context.WithValue(context.Background(), ctxAdminRoles, []string{"moderator"})
	if roles := adminRolesFrom(ctx); len(roles) != 1 || roles[0] != "moderator" {
		t.Errorf("adminRolesFrom = %v", roles)
	}
}
