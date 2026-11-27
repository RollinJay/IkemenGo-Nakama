package main

import "testing"

func TestKeyboardCoverPixels(t *testing.T) {
	for _, tc := range []struct {
		covered, height, pixels, want int32
	}{
		{0, 390, 1170, 0},      // no keyboard
		{-5, 390, 1170, 0},     // nonsense
		{162, 390, 1170, 486},  // iPhone, 3x
		{100, 0, 1170, 0},      // no view yet
		{100, 390, 0, 0},       // no drawable yet
		{390, 390, 1170, 1170}, // all of it
		{500, 390, 1170, 1170}, // more than all of it
		{1, 3, 2, 1},           // rounds to the nearest pixel
		{398, 1024, 2048, 796}, // iPad, 2x
	} {
		if got := keyboardCoverPixels(tc.covered, tc.height, tc.pixels); got != tc.want {
			t.Errorf("keyboardCoverPixels(%d, %d, %d) = %d, want %d", tc.covered, tc.height, tc.pixels, got, tc.want)
		}
	}
}
