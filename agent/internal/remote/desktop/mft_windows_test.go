//go:build windows

package desktop

import "testing"

func TestIMFTransformVTableOffsets(t *testing.T) {
	// IMFTransform derives from IUnknown. These slots are fixed by the Windows
	// ABI and must never be shifted when new constants are added nearby.
	want := map[string]int{
		"GetOutputStreamInfo": vtblGetOutputStreamInfo,
		"GetAttributes":       vtblGetAttributes,
		"GetOutputAvailType":  vtblGetOutputAvailType,
		"SetInputType":        vtblSetInputType,
		"SetOutputType":       vtblSetOutputType,
		"ProcessMessage":      vtblProcessMessage,
		"ProcessInput":        vtblProcessInput,
		"ProcessOutput":       vtblProcessOutput,
	}
	expected := map[string]int{
		"GetOutputStreamInfo": 7,
		"GetAttributes":       8,
		"GetOutputAvailType":  12,
		"SetInputType":        13,
		"SetOutputType":       14,
		"ProcessMessage":      21,
		"ProcessInput":        22,
		"ProcessOutput":       23,
	}
	for name, got := range want {
		if got != expected[name] {
			t.Errorf("IMFTransform %s slot = %d, want %d", name, got, expected[name])
		}
	}
}

func TestMFTCandidateReportsHardwareBeforeNegotiation(t *testing.T) {
	enc := &mftEncoder{hardwareCandidate: true}
	if !enc.IsHardware() {
		t.Fatal("a selected hardware MFT must report hardware before media-type negotiation")
	}
}

func TestMFTWithoutHardwareCandidateIsNotHardware(t *testing.T) {
	enc := &mftEncoder{}
	if enc.IsHardware() {
		t.Fatal("an unselected MFT must not report hardware")
	}
}
