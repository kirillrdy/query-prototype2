package video

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func TestDetectionLetterboxRGBAndPadding(t *testing.T) {
	img := image.NewRGBA(image.Rect(3, 5, 5, 6))
	img.Set(3, 5, color.RGBA{R: 255, A: 255})
	img.Set(4, 5, color.RGBA{R: 255, A: 255})
	data, err := letterboxPixels(img, 4)
	if err != nil {
		t.Fatal(err)
	}
	for channel := 0; channel < 3; channel++ {
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				want := float32(114.0 / 255)
				if y == 1 || y == 2 {
					want = 0
					if channel == 0 {
						want = 1
					}
				}
				if data[channel*16+y*4+x] != want {
					t.Fatalf("channel=%d x=%d y=%d: %v", channel, x, y, data)
				}
			}
		}
	}
}

func TestDetectionCountsClassAwareSuppression(t *testing.T) {
	const n = 5
	data := make([]float32, 84*n)
	// Overlapping people merge; a car at the same position stays; distant
	// people stay; a weak detection is discarded.
	for i := 0; i < n; i++ {
		data[i] = 100
		data[n+i] = 100
		data[2*n+i] = 40
		data[3*n+i] = 40
	}
	data[4*n] = 0.9
	data[4*n+1] = 0.8
	data[6*n+2] = 0.85
	data[3] = 300
	data[4*n+3] = 0.7
	data[4*n+4] = 0.2
	counts, err := detectionCounts(data, []int64{1, 84, n}, 0.25)
	if err != nil || counts["person"] != 2 || counts["car"] != 1 || counts["bus"] != 0 {
		t.Fatalf("%v %v", counts, err)
	}
	// An export with NMS embedded returns xyxy, score, class.
	counts, err = detectionCounts([]float32{1, 2, 10, 20, 0.9, 0}, []int64{1, 1, 6}, 0.25)
	if err != nil || counts["person"] != 1 {
		t.Fatalf("%v %v", counts, err)
	}
}

func TestDetectionCountsRejectsInvalidOutput(t *testing.T) {
	for _, tc := range []struct {
		data  []float32
		shape []int64
	}{
		{nil, []int64{1, 84, 10}},
		{make([]float32, 10), []int64{1, 2, 5}},
		{[]float32{1, 2, 10, 20, 0.9, 80}, []int64{1, 1, 6}},
		{[]float32{1, 2, 10, 20, 0.9, 0.5}, []int64{1, 1, 6}},
		{[]float32{1, 2, 10, 20, float32(math.NaN()), 0}, []int64{1, 1, 6}},
	} {
		if _, err := detectionCounts(tc.data, tc.shape, 0.25); err == nil {
			t.Fatalf("accepted shape=%v data=%v", tc.shape, tc.data)
		}
	}
}
