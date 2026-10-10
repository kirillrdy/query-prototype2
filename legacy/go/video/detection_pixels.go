package video

import (
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"sort"
)

// COCO labels in the default model's class-index order.
var detectionClasses = []string{
	"person", "bicycle", "car", "motorcycle", "airplane", "bus", "train", "truck", "boat", "traffic light",
	"fire hydrant", "stop sign", "parking meter", "bench", "bird", "cat", "dog", "horse", "sheep", "cow",
	"elephant", "bear", "zebra", "giraffe", "backpack", "umbrella", "handbag", "tie", "suitcase", "frisbee",
	"skis", "snowboard", "sports ball", "kite", "baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket", "bottle",
	"wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple", "sandwich", "orange",
	"broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair", "couch", "potted plant", "bed",
	"dining table", "toilet", "tv", "laptop", "mouse", "remote", "keyboard", "cell phone", "microwave", "oven",
	"toaster", "sink", "refrigerator", "book", "clock", "vase", "scissors", "teddy bear", "hair drier", "toothbrush",
}

func detectionPixels(r io.Reader) ([]float32, error) {
	img, err := jpeg.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("decode detection image: %w", err)
	}
	return letterboxPixels(img, 640)
}

// RGB, NCHW, bilinear resize with aspect ratio preserved and centered 114 padding.
func letterboxPixels(img image.Image, size int) ([]float32, error) {
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || b.Dx() > 1280 || b.Dy() > 1280 || size <= 0 || size > 640 {
		return nil, fmt.Errorf("invalid detection image dimensions")
	}
	src := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	width, height := b.Dx(), b.Dy()
	scale := math.Min(float64(size)/float64(width), float64(size)/float64(height))
	w, h := int(math.Round(float64(width)*scale)), int(math.Round(float64(height)*scale))
	w, h = max(1, w), max(1, h)
	left, top := (size-w)/2, (size-h)/2
	data := make([]float32, 3*size*size)
	for i := range data {
		data[i] = 114.0 / 255
	}
	for y := 0; y < h; y++ {
		sy := max(0.0, (float64(y)+0.5)*float64(height)/float64(h)-0.5)
		y0 := min(int(sy), height-1)
		y1 := min(y0+1, height-1)
		dy := float32(sy - float64(y0))
		for x := 0; x < w; x++ {
			sx := max(0.0, (float64(x)+0.5)*float64(width)/float64(w)-0.5)
			x0 := min(int(sx), width-1)
			x1 := min(x0+1, width-1)
			dx := float32(sx - float64(x0))
			for channel := 0; channel < 3; channel++ {
				a := float32(src.Pix[y0*src.Stride+x0*4+channel])
				b := float32(src.Pix[y0*src.Stride+x1*4+channel])
				c := float32(src.Pix[y1*src.Stride+x0*4+channel])
				d := float32(src.Pix[y1*src.Stride+x1*4+channel])
				value := (a+(b-a)*dx)*(1-dy) + (c+(d-c)*dx)*dy
				data[channel*size*size+(top+y)*size+left+x] = value / 255
			}
		}
	}
	return data, nil
}

type detectionBox struct {
	x1, y1, x2, y2, score float32
	class                 int
}

func detectionCounts(data []float32, shape []int64, confidence float32) (map[string]int, error) {
	if len(shape) != 3 || shape[0] != 1 || shape[1] <= 0 || shape[2] <= 0 || shape[1] > 100000 || shape[2] > 100000 || shape[1]*shape[2] != int64(len(data)) {
		return nil, fmt.Errorf("invalid YOLO detection output shape %v", shape)
	}
	boxes := []detectionBox{}
	switch {
	case shape[1] == 84:
		n := int(shape[2])
		for i := 0; i < n; i++ {
			class, score := 0, data[4*n+i]
			for c := 1; c < 80; c++ {
				if s := data[(4+c)*n+i]; s > score {
					class, score = c, s
				}
			}
			if score < confidence {
				continue
			}
			x, y, w, h := data[i], data[n+i], data[2*n+i], data[3*n+i]
			boxes = append(boxes, detectionBox{x - w/2, y - h/2, x + w/2, y + h/2, score, class})
		}
	case shape[2] == 6:
		for i := 0; i < len(data); i += 6 {
			if data[i+4] < confidence {
				continue
			}
			c := data[i+5]
			if !finite(c) || c < 0 || c >= 80 || c != float32(int(c)) {
				return nil, fmt.Errorf("invalid YOLO class index")
			}
			boxes = append(boxes, detectionBox{data[i], data[i+1], data[i+2], data[i+3], data[i+4], int(c)})
		}
	default:
		return nil, fmt.Errorf("expected a COCO YOLO output [1,84,N] or [1,N,6], got %v", shape)
	}
	for _, b := range boxes {
		if !finite(b.x1) || !finite(b.y1) || !finite(b.x2) || !finite(b.y2) || !finite(b.score) || b.score > 1 || b.x2 <= b.x1 || b.y2 <= b.y1 {
			return nil, fmt.Errorf("invalid YOLO detection values")
		}
	}
	sort.SliceStable(boxes, func(i, j int) bool { return boxes[i].score > boxes[j].score })
	counts := map[string]int{}
	for _, name := range detectionClasses {
		counts[name] = 0
	}
	selected := make([]detectionBox, 0, min(300, len(boxes)))
	for _, box := range boxes {
		suppressed := false
		for _, earlier := range selected {
			if earlier.class == box.class && detectionIoU(earlier, box) > 0.7 {
				suppressed = true
				break
			}
		}
		if suppressed {
			continue
		}
		selected = append(selected, box)
		counts[detectionClasses[box.class]]++
		if len(selected) == 300 {
			break
		}
	}
	return counts, nil
}

func finite(v float32) bool { return !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0) }

func detectionIoU(a, b detectionBox) float32 {
	w := max(float32(0), min(a.x2, b.x2)-max(a.x1, b.x1))
	h := max(float32(0), min(a.y2, b.y2)-max(a.y1, b.y1))
	intersection := w * h
	union := (a.x2-a.x1)*(a.y2-a.y1) + (b.x2-b.x1)*(b.y2-b.y1) - intersection
	if union <= 0 {
		return 0
	}
	return intersection / union
}
