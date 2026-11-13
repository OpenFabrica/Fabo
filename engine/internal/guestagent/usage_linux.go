package guestagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/libracoder/kiri/engine/internal/diskusage"
)

func measureUsage(ctx context.Context, layers *LayerStore, runtime *Runtime) []string {
	var lines []string
	measure := func(kind, root, child string) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			name := entry.Name()
			if !entry.IsDir() || strings.ContainsAny(name, " \t\r\n") {
				continue
			}
			path := filepath.Join(root, name, child)
			if size, err := diskusage.Allocated(ctx, path); err == nil {
				lines = append(lines, fmt.Sprintf("%s %s %d", kind, name, size))
			}
		}
	}
	if layers != nil {
		measure("l", layers.dir, "")
	}
	if runtime != nil {
		measure("c", runtime.dir, "upper")
		if runtime.vm && runtime.volumeRoot != "" {
			measure("v", runtime.volumeRoot, "_data")
		}
	}
	return lines
}
