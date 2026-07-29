package builder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStorageUploader(t *testing.T) {
	tests := []struct {
		name          string
		storageType   string
		s3Bucket      string
		s3Region      string
		httpBase      string
		expectError   bool
		expectEnabled bool
	}{
		{
			name:          "local storage",
			storageType:   "local",
			expectError:   false,
			expectEnabled: false,
		},
		{
			name:          "empty storage type",
			storageType:   "",
			expectError:   false,
			expectEnabled: false,
		},
		{
			name:          "s3 without bucket",
			storageType:   "s3",
			expectError:   true,
			expectEnabled: false,
		},
		{
			name:          "s3 with bucket",
			storageType:   "s3",
			s3Bucket:      "test-bucket",
			s3Region:      "us-east-1",
			expectEnabled: true,
		},
		{
			name:          "http without base url",
			storageType:   "http",
			expectError:   true,
			expectEnabled: false,
		},
		{
			name:        "http with base url",
			storageType: "http",
			httpBase:    "http://example.com",
			// HTTP backend is not implemented: construction fails fast.
			expectError: true,
		},
		{
			name:        "unsupported type",
			storageType: "ftp",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uploader, err := NewStorageUploader(
				tt.storageType,
				"/tmp/local",
				tt.s3Bucket,
				tt.s3Region,
				"",
				tt.httpBase,
			)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if uploader == nil {
					t.Fatal("uploader is nil")
				}
				if uploader.IsEnabled() != tt.expectEnabled {
					t.Errorf("IsEnabled() = %v, want %v", uploader.IsEnabled(), tt.expectEnabled)
				}
			}
		})
	}
}

func TestStorageUploaderUploadDisabled(t *testing.T) {
	uploader, err := NewStorageUploader("local", "/tmp/local", "", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	err = uploader.Upload(testFile, "remote/test.txt")
	if err != nil {
		t.Errorf("upload should succeed when disabled: %v", err)
	}
}

func TestStorageUploaderGetURL(t *testing.T) {
	tests := []struct {
		name        string
		storageType string
		s3Bucket    string
		s3Region    string
		httpBase    string
		remotePath  string
		expectError bool
	}{
		{
			name:        "local storage",
			storageType: "local",
			remotePath:  "/path/to/file.txt",
			expectError: false,
		},
		{
			name:        "s3 storage",
			storageType: "s3",
			s3Bucket:    "test-bucket",
			s3Region:    "us-east-1",
			remotePath:  "app-misc/jq/jq-1.8.2.gpkg.tar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uploader, err := NewStorageUploader(
				tt.storageType,
				"/tmp/local",
				tt.s3Bucket,
				tt.s3Region,
				"",
				tt.httpBase,
			)
			if err != nil {
				t.Fatalf("unexpected error creating uploader: %v", err)
			}

			url, err := uploader.GetURL(tt.remotePath)
			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if url == "" {
					t.Error("GetURL returned empty string")
				}
			}
		})
	}
}

func TestStorageUploaderIsEnabled(t *testing.T) {
	tests := []struct {
		name        string
		storageType string
		want        bool
	}{
		{"local", "local", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uploader, err := NewStorageUploader(tt.storageType, "", "", "", "", "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := uploader.IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
