package projects

import "testing"

func TestValidateName(t *testing.T) {
	valid := []string{"webapp", "my app", "client app", "a;b", "$(x)", "my.proj"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{"", ".", "..", "a/b", `a\b`, "/abs"}
	for _, n := range invalid {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}
