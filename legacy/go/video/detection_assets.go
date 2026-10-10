package video

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const defaultModelURL = "https://github.com/ultralytics/assets/releases/download/v8.4.0/yolo11n.onnx"
const defaultModelHash = "634279b40c07c6391472c51ad45b81ebc48706a9a1fe72dd3396322acd0c053b"
const runtimeVersion = "1.23.2"

type detectionAsset struct {
	name, url, hash string
	maxSize         int64
}

// Official Microsoft CPU releases, pinned by their release SHA-256 digests.
func runtimeAsset(goos, goarch string) (detectionAsset, string, error) {
	targets := map[string]struct{ name, hash, library string }{
		"linux/amd64":  {"linux-x64", "1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b", "libonnxruntime.so.1.23.2"},
		"linux/arm64":  {"linux-aarch64", "7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c", "libonnxruntime.so.1.23.2"},
		"darwin/amd64": {"osx-x86_64", "d10359e16347b57d9959f7e80a225a5b4a66ed7d7e007274a15cae86836485a6", "libonnxruntime.1.23.2.dylib"},
		"darwin/arm64": {"osx-arm64", "b4d513ab2b26f088c66891dbbc1408166708773d7cc4163de7bdca0e9bbb7856", "libonnxruntime.1.23.2.dylib"},
	}
	t, ok := targets[goos+"/"+goarch]
	if !ok {
		return detectionAsset{}, "", fmt.Errorf("automatic object detection is unsupported on %s/%s", goos, goarch)
	}
	name := "onnxruntime-" + t.name + "-" + runtimeVersion + ".tgz"
	return detectionAsset{name, "https://github.com/microsoft/onnxruntime/releases/download/v" + runtimeVersion + "/" + name, t.hash, 128 << 20}, t.library, nil
}

var detectionDownloadGate = make(chan struct{}, 1)

func detectionAssets(ctx context.Context, model, cacheDir string) (string, string, error) {
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", "", err
		}
		cacheDir = filepath.Join(base, "binaryquery", "detection")
	}
	select {
	case detectionDownloadGate <- struct{}{}:
		defer func() { <-detectionDownloadGate }()
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	asset, library, err := runtimeAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", "", err
	}
	if model != "" {
		if !strings.EqualFold(filepath.Ext(model), ".onnx") {
			return "", "", fmt.Errorf("BQUERY_DETECTION_MODEL must be an ONNX detection model; unset it to use the automatic default")
		}
		stat, err := os.Stat(model)
		if err != nil {
			return "", "", fmt.Errorf("object detection model: %w", err)
		}
		if !stat.Mode().IsRegular() {
			return "", "", fmt.Errorf("object detection model must be a regular file")
		}
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return "", "", fmt.Errorf("object detection cache: %w", err)
	}
	if model == "" {
		model, err = downloadDetectionAsset(ctx, cacheDir, detectionAsset{"yolo11n-" + defaultModelHash[:12] + ".onnx", defaultModelURL, defaultModelHash, 32 << 20})
		if err != nil {
			return "", "", err
		}
	}
	archive, err := downloadDetectionAsset(ctx, cacheDir, asset)
	if err != nil {
		return "", "", err
	}
	lib, err := extractDetectionLibrary(ctx, archive, cacheDir, library)
	return model, lib, err
}

func fileDigest(path string, maxSize int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxSize {
		return "", fmt.Errorf("invalid cached asset")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Write to a staging file, validate, and rename only complete downloads. A
// cancellation or corrupt response never leaves a partial model in the cache.
func downloadDetectionAsset(ctx context.Context, cacheDir string, asset detectionAsset) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := filepath.Join(cacheDir, asset.name)
	if digest, err := fileDigest(path, asset.maxSize); err == nil && digest == asset.hash {
		return path, nil
	}
	log.Printf("Preparing object detection: downloading %s", asset.name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.url, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("first-use object detection download (internet required): %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("object detection download %s: HTTP %d", asset.name, response.StatusCode)
	}
	f, err := os.CreateTemp(cacheDir, ".download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(response.Body, asset.maxSize+1))
	if err != nil {
		return "", err
	}
	if n > asset.maxSize {
		return "", fmt.Errorf("object detection download exceeds size limit")
	}
	if hex.EncodeToString(h.Sum(nil)) != asset.hash {
		return "", fmt.Errorf("object detection download checksum mismatch for %s", asset.name)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// Extract only the expected regular library; archive paths are never used as
// filesystem destinations. Its content hash becomes the cache filename.
func extractDetectionLibrary(ctx context.Context, archive, cacheDir, library string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer z.Close()
	t := tar.NewReader(z)
	wanted := strings.TrimSuffix(filepath.Base(archive), ".tgz") + "/lib/" + library
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		header, err := t.Next()
		if err == io.EOF {
			return "", fmt.Errorf("ONNX Runtime archive has no %s", library)
		}
		if err != nil {
			return "", err
		}
		if header.Name != wanted {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 128<<20 {
			return "", fmt.Errorf("invalid ONNX Runtime library in archive")
		}
		staging, err := os.CreateTemp(cacheDir, ".library-")
		if err != nil {
			return "", err
		}
		defer os.Remove(staging.Name())
		defer staging.Close()
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(staging, h), t); err != nil {
			return "", err
		}
		digest := hex.EncodeToString(h.Sum(nil))
		path := filepath.Join(cacheDir, digest[:12]+"-"+library)
		if current, err := fileDigest(path, 128<<20); err == nil && current == digest {
			return path, nil
		}
		if err := staging.Close(); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := os.Rename(staging.Name(), path); err != nil {
			return "", err
		}
		return path, nil
	}
}
