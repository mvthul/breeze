package desktop

import "testing"

func TestBackendPreferenceMatches(t *testing.T) {
	tests := []struct {
		preference string
		vendor     string
		want       bool
	}{
		{"", "nvidia", true},
		{"auto", "", true},
		{"mft", "", true},
		{"mft", "intel", false},
		{"qsv", "intel", true},
		{"qsv", "nvidia", false},
		{"nvenc", "nvidia", true},
		{"amf", "amd", true},
		{"openh264", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.preference+"/"+tt.vendor, func(t *testing.T) {
			if got := backendPreferenceMatches(tt.preference, taggedFactory{vendor: tt.vendor}); got != tt.want {
				t.Fatalf("backendPreferenceMatches(%q, %q) = %v, want %v", tt.preference, tt.vendor, got, tt.want)
			}
		})
	}
}

func TestValidateConfigRejectsUnknownBackendPreference(t *testing.T) {
	cfg := DefaultEncoderConfig()
	cfg.BackendPreference = "bogus"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected unknown backend preference to be rejected")
	}
}

