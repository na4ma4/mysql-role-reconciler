package migrate

import (
	"context"
	"fmt"
)

// Storage abstracts read/write operations for state and history persistence.
// Implementations store files under a base location (local directory or S3 prefix).
// All paths are relative to that base location.
type Storage interface {
	// ReadFile reads a file at the given relative path and returns its contents.
	ReadFile(ctx context.Context, relPath string) ([]byte, error)

	// WriteFile writes data to the given relative path, creating parent
	// directories/prefixes as needed.
	WriteFile(ctx context.Context, relPath string, data []byte) error

	// ListFiles returns the names of files under the given relative prefix.
	// The names are relative to prefix (i.e., just the filename portion).
	// Directories/prefixes that are not leaf objects are excluded.
	ListFiles(ctx context.Context, prefix string) ([]string, error)
}

// StorageConfig holds configuration for creating a Storage backend.
type StorageConfig struct {
	Type string   `yaml:"type"` // "local" (default) or "s3"
	Dir  string   `yaml:"dir"`  // base directory for local storage
	S3   S3Config `yaml:"s3"`   // S3 configuration
}

// S3Config holds S3-specific storage configuration.
type S3Config struct {
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
	Region string `yaml:"region"`
}

// NewStorage creates a Storage implementation based on the configuration.
func NewStorage(ctx context.Context, cfg StorageConfig) (Storage, error) {
	switch cfg.Type {
	case "local", "":
		dir := cfg.Dir
		if dir == "" {
			dir = "."
		}
		return NewLocalStorage(dir), nil
	case "s3":
		return NewS3Storage(ctx, cfg.S3)
	default:
		return nil, fmt.Errorf("unknown storage type: %q", cfg.Type)
	}
}
