package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const defaultS3RequestTimeout = 2 * time.Minute

var (
	// ErrObjectConflict means an immutable key already exists with different
	// content. Callers must publish a new generation instead of overwriting it.
	ErrObjectConflict = errors.New("immutable object already exists with different content")
	// ErrDeleteDisabled prevents application runtimes from silently weakening
	// retention. Lifecycle deletion must use an explicitly privileged client.
	ErrDeleteDisabled = errors.New("S3 object deletion is disabled")
	// ErrCompareAndSwapConflict means a channel pointer changed after it was
	// read. Callers must reload and re-evaluate the selected generation.
	ErrCompareAndSwapConflict = errors.New("object compare-and-swap conflict")
)

type s3Client interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// S3Config defines the S3-compatible object authority. Credentials use the
// standard AWS SDK chain (environment, shared config, workload identity).
type S3Config struct {
	Bucket         string
	Region         string
	Prefix         string
	Endpoint       string
	UsePathStyle   bool
	PublicBaseURL  string
	AllowDelete    bool
	RequestTimeout time.Duration
}

// S3Storage stores immutable objects in AWS S3 or an S3-compatible service.
type S3Storage struct {
	client         s3Client
	bucket         string
	prefix         string
	publicBaseURL  string
	allowDelete    bool
	requestTimeout time.Duration
}

// NewS3Storage preserves the original constructor for callers using AWS's
// default endpoint and credential chain.
func NewS3Storage(bucket, region, prefix string) (*S3Storage, error) {
	return NewS3StorageWithConfig(S3Config{
		Bucket: bucket,
		Region: region,
		Prefix: prefix,
	})
}

// NewS3StorageWithConfig creates an immutable S3-compatible backend.
func NewS3StorageWithConfig(cfg S3Config) (*S3Storage, error) {
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Region = strings.TrimSpace(cfg.Region)
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("S3 region is required")
	}
	prefix, err := cleanS3Prefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	endpoint, err := validateBaseURL("S3 endpoint", cfg.Endpoint, true)
	if err != nil {
		return nil, err
	}
	publicBaseURL, err := validateBaseURL("S3 public base URL", cfg.PublicBaseURL, false)
	if err != nil {
		return nil, err
	}

	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = cfg.UsePathStyle
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultS3RequestTimeout
	}
	return &S3Storage{
		client:         client,
		bucket:         cfg.Bucket,
		prefix:         prefix,
		publicBaseURL:  publicBaseURL,
		allowDelete:    cfg.AllowDelete,
		requestTimeout: timeout,
	}, nil
}

// Upload creates an immutable object. Repeating an upload with identical bytes
// is idempotent; attempting to replace the key with different bytes fails.
func (s *S3Storage) Upload(localPath, remotePath string) error {
	key, err := s.objectKey(remotePath)
	if err != nil {
		return err
	}
	file, err := os.Open(localPath) // #nosec G304 -- source path is selected by the caller.
	if err != nil {
		return fmt.Errorf("open S3 upload source: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat S3 upload source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("S3 upload source must be a regular file")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return fmt.Errorf("hash S3 upload source: %w", err)
	}
	sum := digest.Sum(nil)
	sumHex := hex.EncodeToString(sum)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind S3 upload source: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	defer cancel()
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		Body:           file,
		ContentLength:  aws.Int64(info.Size()),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum)),
		IfNoneMatch:    aws.String("*"),
		Metadata: map[string]string{
			"sha256": sumHex,
		},
	})
	if err == nil {
		return nil
	}
	if !isConditionalConflict(err) {
		return fmt.Errorf("put immutable S3 object %q: %w", key, err)
	}
	same, compareErr := s.objectDigestMatches(key, sumHex)
	if compareErr != nil {
		return fmt.Errorf("compare existing immutable S3 object %q: %w", key, compareErr)
	}
	if same {
		return nil
	}
	return fmt.Errorf("%w: s3://%s/%s", ErrObjectConflict, s.bucket, key)
}

// Download atomically materializes an object and verifies its recorded SHA-256.
func (s *S3Storage) Download(remotePath, localPath string) error {
	_, err := s.downloadObject(remotePath, localPath)
	return err
}

// DownloadVersion materializes an object and returns the backend version
// token (the S3 ETag) required by CompareAndSwap.
func (s *S3Storage) DownloadVersion(remotePath, localPath string) (string, error) {
	version, err := s.downloadObject(remotePath, localPath)
	if err != nil {
		return "", err
	}
	if version == "" {
		return "", fmt.Errorf("S3 object did not return an ETag")
	}
	return version, nil
}

func (s *S3Storage) downloadObject(remotePath, localPath string) (string, error) {
	key, err := s.objectKey(remotePath)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	defer cancel()
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", fmt.Errorf("get S3 object %q: %w", key, err)
	}
	defer func() { _ = output.Body.Close() }()

	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0o750); err != nil {
		return "", fmt.Errorf("create S3 download directory: %w", err)
	}
	temp, err := os.CreateTemp(localDir, ".portage-s3-download-*")
	if err != nil {
		return "", fmt.Errorf("create S3 download temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temp, digest), output.Body); err != nil {
		return "", fmt.Errorf("download S3 object %q: %w", key, err)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("sync S3 download: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close S3 download: %w", err)
	}
	if expected := metadataValue(output.Metadata, "sha256"); expected != "" &&
		!strings.EqualFold(expected, hex.EncodeToString(digest.Sum(nil))) {
		return "", fmt.Errorf("S3 object %q SHA-256 mismatch", key)
	}
	if err := os.Rename(tempPath, localPath); err != nil {
		return "", fmt.Errorf("install S3 download: %w", err)
	}
	version := strings.TrimSpace(aws.ToString(output.ETag))
	return version, nil
}

// CompareAndSwap updates one small mutable pointer only when its ETag still
// matches expectedVersion. An empty expectedVersion creates the pointer.
func (s *S3Storage) CompareAndSwap(
	localPath, remotePath, expectedVersion string,
) (string, error) {
	key, err := s.objectKey(remotePath)
	if err != nil {
		return "", err
	}
	file, err := os.Open(localPath) // #nosec G304 -- source path is selected by the caller.
	if err != nil {
		return "", fmt.Errorf("open S3 CAS source: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("S3 CAS source must be a regular file")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash S3 CAS source: %w", err)
	}
	sum := digest.Sum(nil)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	input := &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		Body:           file,
		ContentLength:  aws.Int64(info.Size()),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum)),
		Metadata: map[string]string{
			"sha256": hex.EncodeToString(sum),
		},
	}
	if strings.TrimSpace(expectedVersion) == "" {
		input.IfNoneMatch = aws.String("*")
	} else {
		input.IfMatch = aws.String(strings.TrimSpace(expectedVersion))
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	defer cancel()
	output, err := s.client.PutObject(ctx, input)
	if err != nil {
		if isConditionalConflict(err) {
			return "", fmt.Errorf("%w: s3://%s/%s",
				ErrCompareAndSwapConflict, s.bucket, key)
		}
		return "", fmt.Errorf("compare-and-swap S3 object %q: %w", key, err)
	}
	version := strings.TrimSpace(aws.ToString(output.ETag))
	if version == "" {
		return "", fmt.Errorf("S3 CAS object %q did not return an ETag", key)
	}
	return version, nil
}

// Delete removes an object only for a separately configured lifecycle client.
func (s *S3Storage) Delete(remotePath string) error {
	if !s.allowDelete {
		return ErrDeleteDisabled
	}
	key, err := s.objectKey(remotePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	defer cancel()
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete S3 object %q: %w", key, err)
	}
	return nil
}

// List lists object paths relative to the configured prefix.
func (s *S3Storage) List(prefix string) ([]string, error) {
	relativePrefix, err := cleanS3ListPrefix(prefix)
	if err != nil {
		return nil, err
	}
	fullPrefix := s.prefix + relativePrefix
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	}
	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	files := make([]string, 0)
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	defer cancel()
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list S3 prefix %q: %w", fullPrefix, err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, s.prefix) {
				continue
			}
			files = append(files, strings.TrimPrefix(key, s.prefix))
		}
	}
	sort.Strings(files)
	return files, nil
}

// GetURL returns either the configured read-only public URL or an s3:// URI.
func (s *S3Storage) GetURL(remotePath string) (string, error) {
	key, err := s.objectKey(remotePath)
	if err != nil {
		return "", err
	}
	if s.publicBaseURL != "" {
		return s.publicBaseURL + "/" + escapeObjectKey(key), nil
	}
	return (&url.URL{Scheme: "s3", Host: s.bucket, Path: "/" + key}).String(), nil
}

// Exists checks whether an object exists.
func (s *S3Storage) Exists(remotePath string) (bool, error) {
	key, err := s.objectKey(remotePath)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	defer cancel()
	_, err = s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("head S3 object %q: %w", key, err)
}

func (s *S3Storage) objectDigestMatches(key, expected string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	defer cancel()
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return false, err
	}
	actual := metadataValue(output.Metadata, "sha256")
	return actual != "" && strings.EqualFold(actual, expected), nil
}

func (s *S3Storage) objectKey(remotePath string) (string, error) {
	name, err := cleanRemotePath(filepath.ToSlash(remotePath))
	if err != nil {
		return "", err
	}
	return s.prefix + filepath.ToSlash(name), nil
}

func cleanS3Prefix(prefix string) (string, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return "", nil
	}
	name, err := cleanRemotePath(prefix)
	if err != nil {
		return "", fmt.Errorf("invalid S3 prefix: %w", err)
	}
	return filepath.ToSlash(name) + "/", nil
}

func cleanS3ListPrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", nil
	}
	name, err := cleanRemotePath(strings.TrimPrefix(filepath.ToSlash(prefix), "/"))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(name), nil
}

func validateBaseURL(label, raw string, allowHTTP bool) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must be an absolute URL without credentials, query, or fragment", label)
	}
	if parsed.Scheme != "https" && (!allowHTTP || parsed.Scheme != "http") {
		return "", fmt.Errorf("%s must use HTTPS", label)
	}
	return raw, nil
}

func escapeObjectKey(key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return path.Join(parts...)
}

func metadataValue(metadata map[string]string, key string) string {
	for name, value := range metadata {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func isConditionalConflict(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode() == "PreconditionFailed" ||
		apiErr.ErrorCode() == "ConditionalRequestConflict"
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey"
}
