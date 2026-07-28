package iam

import (
	"context"
	"reflect"
	"testing"
)

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		actual, required string
		allowed          bool
	}{
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleDeveloper, false},
		{RoleDeveloper, RoleViewer, true},
		{RoleMaintainer, RoleDeveloper, true},
		{RoleOwner, RoleMaintainer, true},
		{"admin", RoleViewer, false},
		{RoleOwner, "unknown", false},
	}
	for _, tc := range cases {
		if got := RoleAtLeast(tc.actual, tc.required); got != tc.allowed {
			t.Fatalf("RoleAtLeast(%q, %q) = %t, want %t",
				tc.actual, tc.required, got, tc.allowed)
		}
	}
}

func TestPrincipalContext(t *testing.T) {
	want := Principal{SubjectID: "subject-id", Subject: "alice"}
	ctx := WithPrincipal(context.Background(), want)
	got, ok := PrincipalFromContext(ctx)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("principal = %+v, %t", got, ok)
	}
}
