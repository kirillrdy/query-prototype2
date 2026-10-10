//go:build (!linux && !darwin) || (!amd64 && !arm64)

package video

import (
	"context"
	"fmt"
	"runtime"
)

func openDetectionBackend(context.Context, string, string) (detectionBackend, error) {
	return nil, fmt.Errorf("object detection is currently supported on Linux and macOS, amd64 and arm64 (this platform is %s/%s)", runtime.GOOS, runtime.GOARCH)
}
