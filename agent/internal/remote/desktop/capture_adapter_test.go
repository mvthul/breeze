package desktop

import "testing"

func TestAdapterIdentityVendor(t *testing.T) {
	tests := []struct {
		name string
		id   uint32
		want string
	}{
		{"nvidia", 0x10de, "nvidia"},
		{"intel", 0x8086, "intel"},
		{"amd", 0x1002, "amd"},
		{"unknown", 0xffff, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (AdapterIdentity{VendorID: tt.id}).Vendor(); got != tt.want {
				t.Fatalf("Vendor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHardwareFactoryEligible(t *testing.T) {
	intel := &AdapterIdentity{VendorID: 0x8086, LUID: 1}
	nvidia := &AdapterIdentity{VendorID: 0x10de, LUID: 2}
	for _, tc := range []struct {
		name   string
		vendor string
		cap    *AdapterIdentity
		want   bool
	}{
		{"generic with capture", "", intel, true},
		{"generic without capture", "", nil, true},
		{"matching vendor", "intel", intel, true},
		{"mismatching vendor", "nvidia", intel, false},
		{"different adapter is still rejected by vendor", "intel", nvidia, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hardwareFactoryEligible(tc.vendor, tc.cap); got != tc.want {
				t.Fatalf("hardwareFactoryEligible(%q, %#v) = %v, want %v", tc.vendor, tc.cap, got, tc.want)
			}
		})
	}
}
