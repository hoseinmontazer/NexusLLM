package modelguard

import "testing"

func TestEligible(t *testing.T) {
	cases := []struct {
		name      string
		enabled   bool
		lifecycle string
		want      bool
	}{
		{"enabled and active", true, "active", true},
		{"enabled and empty lifecycle", true, "", true},
		{"disabled but active lifecycle", false, "active", false},
		{"enabled but soft-deleted lifecycle", true, "deleted", false},
		{"disabled and soft-deleted", false, "deleted", false},
		{"enabled and archived", true, "archived", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Eligible(c.enabled, c.lifecycle); got != c.want {
				t.Fatalf("Eligible(%v, %q) = %v, want %v", c.enabled, c.lifecycle, got, c.want)
			}
		})
	}
}
