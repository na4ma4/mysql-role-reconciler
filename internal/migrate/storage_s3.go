package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Storage implements Storage using AWS S3.
type S3Storage struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Storage creates a Storage backed by S3.
// The prefix is prepended to all object keys (should end with "/" if non-empty).
func NewS3Storage(ctx context.Context, cfg S3Config) (*S3Storage, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3 storage requires a bucket")
	}

	var opts []func(*config.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}

	cfgAWS, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfgAWS)

	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return &S3Storage{
		client: client,
		bucket: cfg.Bucket,
		prefix: prefix,
	}, nil
}

func (s *S3Storage) objectKey(relPath string) string {
	return s.prefix + relPath
}

// ReadFile reads an object from S3.
func (s *S3Storage) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(relPath)),
	})
	if err != nil {
		return nil, fmt.Errorf("reading s3 object %s: %w", relPath, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading s3 object body %s: %w", relPath, err)
	}

	return data, nil
}

// WriteFile writes data to an S3 object.
func (s *S3Storage) WriteFile(ctx context.Context, relPath string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(relPath)),
		Body:   strings.NewReader(string(data)),
	})
	if err != nil {
		return fmt.Errorf("writing s3 object %s: %w", relPath, err)
	}

	return nil
}

// ListFiles returns the names of objects under the given prefix in S3.
// Only the base name of each object key (after the prefix) is returned.
// "Directory" objects (keys ending with "/") are excluded.
func (s *S3Storage) ListFiles(ctx context.Context, prefix string) ([]string, error) {
	fullPrefix := s.objectKey(prefix)
	if fullPrefix != "" && !strings.HasSuffix(fullPrefix, "/") {
		fullPrefix += "/"
	}

	var names []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing s3 objects under %s: %w", prefix, err)
		}

		for _, obj := range page.Contents {
			key := *obj.Key
			if strings.HasSuffix(key, "/") {
				continue // skip directory markers
			}
			// Strip the full prefix to get just the filename
			name := strings.TrimPrefix(key, fullPrefix)
			if name == key {
				continue // prefix didn't match
			}
			if strings.HasPrefix(name, ".") {
				continue // skip dotfiles
			}
			names = append(names, name)
		}
	}

	sort.Strings(names)
	return names, nil
}
