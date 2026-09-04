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
	for _, field := range []struct{ name, want, got string }{
		{"ID", expected.ID, observed.ID},
		{"name", expected.Name, observed.Name},
	} {
		if strings.TrimSpace(field.want) == "" || field.got != field.want {
			return &ResourceIdentityMismatch{Field: field.name, Expected: field.want, Observed: field.got}
		}
	}
	return nil
}
