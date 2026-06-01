package projects

import "testing"

func TestValidateName(t *testing.T) {
	valid := []string{"webapp", "my app", "client app", "a;b", "$(x)", "my.proj"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{
		"", ".", "..", "a/b", `a\b`, "/abs",
		"a@b", "a+b", // reserved for the name@tag session format
		"a:b", "a*b", `a"b`, "a<b", "a>b", "a|b", "a?b", // Windows-illegal
		"name.", "name ", // Windows: no trailing dot/space
		"con", "COM1", "nul", // Windows reserved device names
	}
	for _, n := range invalid {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestValidateTag(t *testing.T) {
	for _, ok := range []string{"go", "tools", "v2", "client work"} {
		if err := ValidateTag(ok); err != nil {
			t.Errorf("ValidateTag(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "a@b", "go+lang", "a/b", `a\b`} {
		if err := ValidateTag(bad); err == nil {
			t.Errorf("ValidateTag(%q) = nil, want error", bad)
		}
	}
}
