package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinIO struct {
	client     *minio.Client
	core       *minio.Core
	bucket     string
	region     string
	endpoint   string
	useSSL     bool
	publicBase string
}

type MinIOOptions struct {
	Endpoint      string
	AccessKey     string
	SecretKey     string
	Bucket        string
	Region        string
	UseSSL        bool
	PublicBaseURL string
}

func NewMinIO(opts MinIOOptions) (*MinIO, error) {
	if opts.Endpoint == "" || opts.Bucket == "" {
		return nil, fmt.Errorf("storage endpoint and bucket are required")
	}
	client, err := minio.New(opts.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure: opts.UseSSL,
		Region: opts.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}
	core, err := minio.NewCore(opts.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure: opts.UseSSL,
		Region: opts.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio core: %w", err)
	}
	return &MinIO{
		client:     client,
		core:       core,
		bucket:     opts.Bucket,
		region:     opts.Region,
		endpoint:   opts.Endpoint,
		useSSL:     opts.UseSSL,
		publicBase: strings.TrimRight(opts.PublicBaseURL, "/"),
	}, nil
}

func (m *MinIO) EnsureBucket(ctx context.Context) error {
	ok, err := m.client.BucketExists(ctx, m.bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if ok {
		return nil
	}
	opts := minio.MakeBucketOptions{Region: m.region}
	if err := m.client.MakeBucket(ctx, m.bucket, opts); err != nil {
		// Another instance may have created it concurrently.
		if exists, e := m.client.BucketExists(ctx, m.bucket); e == nil && exists {
			return nil
		}
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

func (m *MinIO) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := m.client.BucketExists(ctx, m.bucket)
	return err
}

func (m *MinIO) PresignPut(ctx context.Context, key string, expires time.Duration) (string, error) {
	u, err := m.client.PresignedPutObject(ctx, m.bucket, key, expires)
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return u.String(), nil
}

func (m *MinIO) PresignGet(ctx context.Context, key string, expires time.Duration) (string, error) {
	u, err := m.client.PresignedGetObject(ctx, m.bucket, key, expires, nil)
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return u.String(), nil
}

// PresignGetNamed signs a download that browsers save under filename. The
// disposition is part of the signature, so it cannot be changed by the client.
func (m *MinIO) PresignGetNamed(ctx context.Context, key, filename string, expires time.Duration) (string, error) {
	params := url.Values{}
	params.Set("response-content-disposition", contentDisposition(filename))
	u, err := m.client.PresignedGetObject(ctx, m.bucket, key, expires, params)
	if err != nil {
		return "", fmt.Errorf("presign get named: %w", err)
	}
	return u.String(), nil
}

func contentDisposition(filename string) string {
	name := strings.TrimSpace(filename)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" {
		name = "download"
	}
	ascii := true
	for _, r := range name {
		if r > 0x7e || r == '"' || r == '\\' {
			ascii = false
			break
		}
	}
	if ascii {
		return `attachment; filename="` + name + `"`
	}
	// RFC 5987: percent-encoded UTF-8 keeps CJK file names intact while the
	// quoted fallback satisfies clients that ignore filename*.
	escaped := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	return `attachment; filename="download"; filename*=UTF-8''` + escaped
}

func (m *MinIO) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := m.client.PutObject(ctx, m.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

func (m *MinIO) CreateMultipart(ctx context.Context, key string) (string, error) {
	id, err := m.core.NewMultipartUpload(ctx, m.bucket, key, minio.PutObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("create multipart upload: %w", err)
	}
	return id, nil
}

func (m *MinIO) PresignPart(ctx context.Context, key, uploadID string, partNumber int32, expires time.Duration) (string, error) {
	params := url.Values{}
	params.Set("partNumber", strconv.Itoa(int(partNumber)))
	params.Set("uploadId", uploadID)
	u, err := m.core.Presign(ctx, http.MethodPut, m.bucket, key, expires, params)
	if err != nil {
		return "", fmt.Errorf("presign part: %w", err)
	}
	return u.String(), nil
}

func (m *MinIO) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	complete := make([]minio.CompletePart, 0, len(parts))
	for _, p := range parts {
		complete = append(complete, minio.CompletePart{PartNumber: int(p.Number), ETag: p.ETag})
	}
	if _, err := m.core.CompleteMultipartUpload(ctx, m.bucket, key, uploadID, complete, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	return nil
}

func (m *MinIO) AbortMultipart(ctx context.Context, key, uploadID string) error {
	return m.core.AbortMultipartUpload(ctx, m.bucket, key, uploadID)
}

func (m *MinIO) Stat(ctx context.Context, key string) (int64, error) {
	info, err := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("stat object: %w", err)
	}
	return info.Size, nil
}

func (m *MinIO) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	return obj, nil
}

func (m *MinIO) Delete(ctx context.Context, key string) error {
	if err := m.client.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil
		}
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func (m *MinIO) PublicURL(key string) string {
	if m.publicBase != "" {
		return m.publicBase + "/" + key
	}
	scheme := "http"
	if m.useSSL {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%s", scheme, m.endpoint, m.bucket, key)
}
