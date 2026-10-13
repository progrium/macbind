package server

import (
	"fmt"
	"path/filepath"
	"strings"
)

// toVFSPath converts an absolute real-fs path (e.g. "/Users/x/foo") into a
// relative path suitable for the wanix vfs (e.g. "Users/x/foo"), whose root
// corresponds to the real filesystem's root.
func toVFSPath(abs string) (string, error) {
	if !filepath.IsAbs(abs) {
		return "", fmt.Errorf("not an absolute path: %s", abs)
	}
	rel := strings.TrimPrefix(filepath.Clean(abs), "/")
	if rel == "" {
		rel = "."
	}
	return rel, nil
}
