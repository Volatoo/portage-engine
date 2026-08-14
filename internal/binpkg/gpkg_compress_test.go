package binpkg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := zstd.NewWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func xzBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := xz.NewWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestGenerateIndexCompressedMetadata(t *testing.T) {
	metadata := writeTar(t, map[string]string{
		"metadata/SLOT":     "3.11",
		"metadata/USE":      "ssl threads sqlite",
		"metadata/KEYWORDS": "amd64",
		"metadata/CATEGORY": "dev-lang",
		"metadata/PF":       "python-3.11.0",
	})

	tests := []struct {
		name   string
		member string
		encode func(*testing.T, []byte) []byte
	}{
		{name: "gzip", member: "metadata.tar.gz", encode: gzipBytes},
		{name: "zstd", member: "metadata.tar.zst", encode: zstdBytes},
		{name: "xz", member: "metadata.tar.xz", encode: xzBytes},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			pkgDir := filepath.Join(dir, "dev-lang", "python")
			if err := os.MkdirAll(pkgDir, 0o755); err != nil {
				t.Fatal(err)
			}

			gpkg := writeTar(t, map[string]string{
				"gpkg-1":                                "",
				"dev-lang/python-3.11.0/" + test.member: string(test.encode(t, metadata)),
				"dev-lang/python-3.11.0/image.tar":      "fake-image",
				"dev-lang/python-3.11.0/Manifest":       "DATA " + test.member + " 1 SHA512 abc",
			})
			if err := os.WriteFile(filepath.Join(pkgDir, "python-3.11.0.gpkg.tar"), gpkg, 0o644); err != nil {
				t.Fatal(err)
			}

			count, err := GenerateIndex(dir, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("expected one package, got %d", count)
			}

			data, err := os.ReadFile(filepath.Join(dir, "Packages"))
			if err != nil {
				t.Fatal(err)
			}
			index := string(data)
			for _, expected := range []string{
				"CPV: dev-lang/python-3.11.0",
				"SLOT: 3.11",
				"USE: ssl threads sqlite",
				"KEYWORDS: amd64",
			} {
				if !strings.Contains(index, expected) {
					t.Errorf("%s metadata index missing %q; got:\n%s", test.name, expected, index)
				}
			}
		})
	}
}
