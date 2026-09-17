package desktop

import "testing"

// TestClampThreads locks in the thread-count policy for OpenH264's
// IMultipleThreadIdc: minimum 2 (so the encoder can overlap slice encode with
// bitstream emit), maximum 4 (realtime H264 sees marginal returns past 4).
func TestClampThreads(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 2},
		{1, 2},
		{2, 2},
		{3, 3},
		{4, 4},
		{8, 4},
		{64, 4},
	}
	for _, tc := range cases {
		if got := clampThreads(tc.in); got != tc.want {
			t.Errorf("clampThreads(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestClampOpenH264DimensionsPreservesAspectAndPixelCeiling(t *testing.T) {
	w, h := clampOpenH264Dimensions(7680, 2160)
	if w%2 != 0 || h%2 != 0 {
		t.Fatalf("dimensions must be even: %dx%d", w, h)
	}
	if w*h > maxOpenH264Pixels {
		t.Fatalf("pixel count %d exceeds OpenH264 ceiling %d", w*h, maxOpenH264Pixels)
	}
	if w >= 7680 || h >= 2160 {
		t.Fatalf("oversized input was not reduced: %dx%d", w, h)
	}
	if got, want := float64(w)/float64(h), 7680.0/2160.0; got < want-0.01 || got > want+0.01 {
		t.Fatalf("aspect ratio changed too much: got %.4f want %.4f", got, want)
	}
}
