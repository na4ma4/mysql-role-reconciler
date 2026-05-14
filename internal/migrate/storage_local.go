package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/na4ma4/go-permbits"
)

// LocalStorage implements Storage using the local filesystem.
type LocalStorage struct {
	baseDir string
}

// NewLocalStorage creates a Storage backed by the local filesystem rooted at baseDir.
func NewLocalStorage(baseDir string) *LocalStorage {
	return &LocalStorage{baseDir: baseDir}
}

func (s *LocalStorage) fullPath(relPath string) string {
	return filepath.Join(s.baseDir, relPath)
}

// ReadFile reads a file from the local filesystem.
func (s *LocalStorage) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	data, err := os.ReadFile(s.fullPath(relPath))
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", relPath, err)
	}
	return data, nil
}

// WriteFile writes data to the local filesystem, creating parent directories as needed.
func (s *LocalStorage) WriteFile(_ context.Context, relPath string, data []byte) error {
	fullPath := s.fullPath(relPath)

	if err := os.MkdirAll(filepath.Dir(fullPath), permbits.MustString("u=rwx,go=rx")); err != nil {
		return fmt.Errorf("creating directory for %s: %w", relPath, err)
	}

	if err := os.WriteFile(fullPath, data, permbits.MustString("u=rw")); err != nil {
		return fmt.Errorf("writing file %s: %w", relPath, err)
	}

	return nil
}

// ListFiles returns the names of files in a directory under baseDir.
// Subdirectories are excluded; only leaf file names are returned.
func (s *LocalStorage) ListFiles(_ context.Context, prefix string) ([]string, error) {
	dir := s.fullPath(prefix)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing directory %s: %w", prefix, err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}

	sort.Strings(names)
	return names, nil
}
