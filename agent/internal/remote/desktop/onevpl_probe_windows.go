//go:build windows

package desktop

import (
	"fmt"
	"syscall"
)

// probeOneVPLRuntime checks only the dispatcher ABI. It deliberately does not
// keep the DLL loaded or claim that an encoder is available: selecting an
// accelerated implementation still requires the encode-session slice. This
// probe gives the fallback log enough information to distinguish "Intel
// runtime is absent" from "Breeze has not yet initialised oneVPL".
func probeOneVPLRuntime() error {
	dll := syscall.NewLazyDLL("vpl.dll")
	if err := dll.Load(); err != nil {
		return fmt.Errorf("load vpl.dll: %w", err)
	}
	for _, symbol := range []string{"MFXLoad", "MFXCreateConfig", "MFXCreateSession"} {
		if err := dll.NewProc(symbol).Find(); err != nil {
			return fmt.Errorf("vpl.dll missing %s: %w", symbol, err)
		}
	}
	return nil
}
