package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func runtimeFileOwner() (int, int, bool) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return 0, 0, false
	}
	return envInt("PROJECT_RUNTIME_FILE_OWNER_UID", 1001), envInt("PROJECT_RUNTIME_FILE_OWNER_GID", 1001), true
}

func setRuntimePathOwner(path string) error {
	uid, gid, ok := runtimeFileOwner()
	if !ok {
		return nil
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("set runtime owner for %s: %w", path, err)
	}
	return nil
}

func setRuntimeOwnerPathChain(root, path string) error {
	uid, gid, ok := runtimeFileOwner()
	if !ok {
		return nil
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside runtime owner root: %s", path)
	}
	current := root
	if err := os.Lchown(current, uid, gid); err != nil {
		return fmt.Errorf("set runtime owner for %s: %w", current, err)
	}
	if rel == "." || rel == "" {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := os.Lchown(current, uid, gid); err != nil {
			return fmt.Errorf("set runtime owner for %s: %w", current, err)
		}
	}
	return nil
}
