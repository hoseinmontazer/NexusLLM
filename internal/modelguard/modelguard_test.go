package modelguard

import (
	"strings"
	"testing"
)

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

func TestManagedByNexus(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"explicit managed", ModeManaged, true},
		{"explicit manual", ModeManual, false},
		// Empty means the column is NULL or migration 061 has not been applied.
		// Defaulting to managed keeps lifecycle management working for every
		// model that relies on it.
		{"empty defaults to managed", "", true},
		{"unknown value defaults to managed", "kubernetes", true},
		// Case matters: the column has a CHECK constraint, so a value that only
		// differs in case cannot be stored — treat it as unknown, not manual.
		{"wrong case is not manual", "MANUAL", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ManagedByNexus(c.mode); got != c.want {
				t.Fatalf("ManagedByNexus(%q) = %v, want %v", c.mode, got, c.want)
			}
		})
	}
}

// SQLManagedCondition must stay equivalent to ManagedByNexus. It cannot be
// evaluated without a database here, so assert the one property every caller
// relies on: it filters on the manual value and nothing else, and it treats a
// NULL column as managed.
func TestSQLManagedConditionMatchesPredicate(t *testing.T) {
	if !strings.Contains(SQLManagedCondition, "'"+ModeManual+"'") {
		t.Errorf("SQLManagedCondition must filter on %q: %s", ModeManual, SQLManagedCondition)
	}
	if !strings.Contains(SQLManagedCondition, "COALESCE") ||
		!strings.Contains(SQLManagedCondition, "'"+ModeManaged+"'") {
		t.Errorf("SQLManagedCondition must COALESCE a NULL column to %q: %s", ModeManaged, SQLManagedCondition)
	}
	if !strings.HasPrefix(SQLManagedCondition, "COALESCE(m.") {
		t.Errorf("SQLManagedCondition must use the documented models alias \"m\": %s", SQLManagedCondition)
	}
}
