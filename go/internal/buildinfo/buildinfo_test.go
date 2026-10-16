package buildinfo

import "testing"

func TestString(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	tests := []struct {
		version string
		want    string
	}{
		{"", "dev"},
		{"0123abc", "0123abc"},
	}
	for _, tt := range tests {
		Version = tt.version
		if got := String(); got != tt.want {
			t.Errorf("String() with Version=%q = %q, want %q", tt.version, got, tt.want)
		}
	}
}
