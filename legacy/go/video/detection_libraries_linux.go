//go:build linux && (amd64 || arm64)

package video

import (
	"path/filepath"
	"strings"

	"github.com/ebitengine/purego"
)

func preloadDetectionLibraries() {
	for _, name := range []string{"libgcc_s.so.1", "libstdc++.so.6"} {
		if _, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			continue
		}
		paths, _ := filepath.Glob("/nix/store/*-gcc-*-lib/lib/" + name)
		for _, path := range paths {
			if strings.Contains(path, "-unknown-") {
				continue
			}
			if _, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
				break
			}
		}
	}
}
