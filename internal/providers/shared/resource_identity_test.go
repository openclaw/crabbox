package shared

import "testing"

func TestValidateResourceID(t *testing.T) {
	for _, tt := range []struct {
		name, expected, observed string
		matches                  bool
	}{
		{"same", "sbx_a", "sbx_a", true},
		{"different", "sbx_a", "sbx_b", false},
		{"missing observed", "sbx_a", "", false},
		{"missing expected", "", "", false},
		{"blank expected", " ", " ", false},
		{"no trimming", "sbx_a", " sbx_a", false},
		{"no case folding", "sbx_a", "SBX_A", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mismatch := ValidateResourceID(tt.expected, tt.observed)
			if tt.matches {
				if mismatch != nil {
					t.Fatal(mismatch)
				}
			} else if mismatch == nil || mismatch.Field != "ID" || mismatch.Expected != tt.expected || mismatch.Observed != tt.observed {
				t.Fatalf("mismatch=%v", mismatch)
			}
		})
	}
}

func TestNamedResourceIdentity(t *testing.T) {
	for _, tt := range []struct {
		name               string
		expected, observed NamedResourceIdentity
		field              string
	}{
		{"same", NamedResourceIdentity{"42", "worker"}, NamedResourceIdentity{"42", "worker"}, ""},
		{"different ID", NamedResourceIdentity{"42", "worker"}, NamedResourceIdentity{"43", "worker"}, "ID"},
		{"different name", NamedResourceIdentity{"42", "worker"}, NamedResourceIdentity{"42", "other"}, "name"},
		{"missing expected ID", NamedResourceIdentity{"", "worker"}, NamedResourceIdentity{"", "worker"}, "ID"},
		{"blank expected name", NamedResourceIdentity{"42", " "}, NamedResourceIdentity{"42", " "}, "name"},
		{"missing observed ID", NamedResourceIdentity{"42", "worker"}, NamedResourceIdentity{"", "worker"}, "ID"},
		{"missing observed name", NamedResourceIdentity{"42", "worker"}, NamedResourceIdentity{"42", ""}, "name"},
		{"no ID trimming", NamedResourceIdentity{"42", "worker"}, NamedResourceIdentity{" 42", "worker"}, "ID"},
		{"no case folding", NamedResourceIdentity{"42", "worker"}, NamedResourceIdentity{"42", "Worker"}, "name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mismatch := tt.expected.Validate(tt.observed)
			if tt.field == "" {
				if mismatch != nil {
					t.Fatal(mismatch)
				}
			} else if mismatch == nil || mismatch.Field != tt.field {
				t.Fatalf("mismatch=%v, want field %s", mismatch, tt.field)
			}
		})
	}
}
