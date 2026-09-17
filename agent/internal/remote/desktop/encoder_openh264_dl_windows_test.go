//go:build windows

package desktop

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOpenH264RuntimeFileACLGrantsUsersReadOnly(t *testing.T) {
	if !strings.HasPrefix(openH264RuntimeFileSDDL, "D:P") {
		t.Fatalf("codec DLL ACL must be protected: %s", openH264RuntimeFileSDDL)
	}
	if !strings.Contains(openH264RuntimeFileSDDL, "(A;;FRFX;;;BU)") {
		t.Fatalf("codec DLL ACL must grant BUILTIN\\Users read+execute: %s", openH264RuntimeFileSDDL)
	}
	if _, err := windows.SecurityDescriptorFromString(openH264RuntimeFileSDDL); err != nil {
		t.Fatalf("codec DLL ACL must parse: %v", err)
	}
}
