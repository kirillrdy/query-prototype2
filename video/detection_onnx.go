//go:build (linux || darwin) && (amd64 || arm64)

package video

import (
	"context"
	"fmt"
	"os"
	"runtime"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

type onnxDetector struct {
	runtime *ort.Runtime
	env     *ort.Env
	session *ort.Session
}

func openDetectionBackend(ctx context.Context, model, cacheDir string) (detectionBackend, error) {
	model, library, err := detectionAssets(ctx, model, cacheDir)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := ort.NewRuntime(library, 23)
	if err != nil {
		// NixOS keeps the C++ standard library outside the loader's usual paths.
		// Resolve its already installed libraries without asking for setup.
		preloadDetectionLibraries()
		r, err = ort.NewRuntime(library, 23)
	}
	if err != nil {
		return nil, fmt.Errorf("load local object detection runtime: %w", err)
	}
	d := &onnxDetector{runtime: r}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()
	d.env, err = r.NewEnv("binaryquery-detection", ort.LoggingLevelError)
	if err != nil {
		return nil, err
	}
	d.session, err = r.NewSession(d.env, model, &ort.SessionOptions{IntraOpNumThreads: min(4, runtime.NumCPU())})
	if err != nil {
		return nil, fmt.Errorf("load object detection model: %w", err)
	}
	if len(d.session.InputNames()) != 1 || len(d.session.OutputNames()) != 1 {
		return nil, fmt.Errorf("expected a single-input, single-output COCO YOLO detection model")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ok = true
	return d, nil
}

func (d *onnxDetector) Close() {
	if d.session != nil {
		d.session.Close()
		d.session = nil
	}
	if d.env != nil {
		d.env.Close()
		d.env = nil
	}
	if d.runtime != nil {
		d.runtime.Close()
		d.runtime = nil
	}
}

func (d *onnxDetector) Count(ctx context.Context, path string, confidence float32) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := detectionPixels(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := ort.NewTensorValue(d.runtime, data, []int64{1, 3, 640, 640})
	if err != nil {
		return nil, err
	}
	defer input.Close()
	outputs, err := d.session.Run(ctx, map[string]*ort.Value{d.session.InputNames()[0]: input})
	// The tensor borrows Go memory; keep it alive through the native call.
	runtime.KeepAlive(data)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, output := range outputs {
			output.Close()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output := outputs[d.session.OutputNames()[0]]
	if output == nil {
		return nil, fmt.Errorf("object detector returned no output")
	}
	values, shape, err := ort.GetTensorData[float32](output)
	if err != nil {
		return nil, err
	}
	return detectionCounts(values, shape, confidence)
}
