package shared

import (
	"fmt"
	"strings"
)

// NamedResourceIdentity binds later observations to an ID-addressed allocation.
// It is not a deletion authorization or a generation fence for reusable names.
type NamedResourceIdentity struct {
	ID, Name string
}

type ResourceIdentityMismatch struct {
	Field, Expected, Observed string
}

func (e *ResourceIdentityMismatch) Error() string {
	return fmt.Sprintf("resource %s mismatch: got %q, want %q", e.Field, e.Observed, e.Expected)
}

// Validate requires a complete expected identity and exact, unnormalized values.
func (expected NamedResourceIdentity) Validate(observed NamedResourceIdentity) *ResourceIdentityMismatch {
	if mismatch := ValidateResourceID(expected.ID, observed.ID); mismatch != nil {
		return mismatch
	}
	if strings.TrimSpace(expected.Name) == "" || observed.Name != expected.Name {
		return &ResourceIdentityMismatch{Field: "name", Expected: expected.Name, Observed: observed.Name}
	}
	return nil
}

// ValidateResourceID binds an observation to a nonblank, exact requested ID.
// It does not establish ownership or authorize resource deletion.
func ValidateResourceID(expected, observed string) *ResourceIdentityMismatch {
	if strings.TrimSpace(expected) == "" || observed != expected {
		return &ResourceIdentityMismatch{Field: "ID", Expected: expected, Observed: observed}
	}
	return nil
}
