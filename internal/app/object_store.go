package app

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type objectStore interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) (objectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, objectInfo, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]objectInfo, error)
}

type objectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

func newObjectStore(cfg Config) (objectStore, error) {
	if strings.TrimSpace(cfg.ObjectStoreType) == "" {
		return nil, nil
	}
	if strings.TrimSpace(cfg.ObjectStoreType) != "s3" {
		return nil, errors.New("PROJECT_RUNTIME_OBJECT_STORE must be s3 when set")
	}
	if strings.TrimSpace(cfg.ObjectStoreBucket) == "" {
		return nil, errors.New("PROJECT_RUNTIME_OBJECT_BUCKET is required for object storage")
	}
	if strings.TrimSpace(cfg.ObjectStoreAccessKey) == "" || strings.TrimSpace(cfg.ObjectStoreSecretKey) == "" {
		return nil, errors.New("PROJECT_RUNTIME_OBJECT_ACCESS_KEY_ID and PROJECT_RUNTIME_OBJECT_SECRET_ACCESS_KEY are required")
	}
	return &s3ObjectStore{
		bucket: strings.TrimSpace(cfg.ObjectStoreBucket),
		client: s3.New(s3.Options{
			Region:       strings.TrimSpace(cfg.ObjectStoreRegion),
			BaseEndpoint: optionalStringPointer(strings.TrimSpace(cfg.ObjectStoreEndpoint)),
			Credentials: credentials.NewStaticCredentialsProvider(
				strings.TrimSpace(cfg.ObjectStoreAccessKey),
				strings.TrimSpace(cfg.ObjectStoreSecretKey),
				"",
			),
			UsePathStyle: cfg.ObjectStorePathStyle,
		}),
	}, nil
}

type s3ObjectStore struct {
	bucket string
	client *s3.Client
}

func (s *s3ObjectStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) (objectInfo, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   optionalStringPointer(contentType),
	})
	if err != nil {
		return objectInfo{}, err
	}
	return objectInfo{Key: key, Size: size, LastModified: time.Now().UTC(), ETag: aws.ToString(out.ETag)}, nil
}

func (s *s3ObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, objectInfo, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, objectInfo{}, err
	}
	info := objectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		ETag:         aws.ToString(out.ETag),
	}
	return out.Body, info, nil
}

func (s *s3ObjectStore) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

func (s *s3ObjectStore) List(ctx context.Context, prefix string) ([]objectInfo, error) {
	var out []objectInfo
	var continuation *string
	for {
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			out = append(out, objectInfo{
				Key:          aws.ToString(object.Key),
				Size:         aws.ToInt64(object.Size),
				LastModified: aws.ToTime(object.LastModified),
				ETag:         aws.ToString(object.ETag),
			})
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			break
		}
		continuation = page.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastModified.After(out[j].LastModified)
	})
	return out, nil
}

func optionalStringPointer(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return aws.String(value)
}

func objectKey(prefix string, parts ...string) string {
	items := make([]string, 0, len(parts)+1)
	if clean := strings.Trim(strings.TrimSpace(prefix), "/"); clean != "" {
		items = append(items, clean)
	}
	for _, part := range parts {
		clean := strings.Trim(strings.TrimSpace(part), "/")
		if clean != "" {
			items = append(items, clean)
		}
	}
	return strings.Join(items, "/")
}
