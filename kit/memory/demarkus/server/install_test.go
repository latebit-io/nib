package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectPlatform(t *testing.T) {
	platform, arch, err := detectPlatform()
	if err != nil {
		t.Fatalf("detectPlatform: %v", err)
	}
	if platform == "" {
		t.Error("platform is empty")
	}
	if arch == "" {
		t.Error("arch is empty")
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello world")
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	t.Run("matching checksum", func(t *testing.T) {
		checksums := fmt.Sprintf("%s  archive.tar.gz\n%s  other.tar.gz\n", hexSum, "deadbeef")
		if err := verifyChecksum(data, "archive.tar.gz", []byte(checksums)); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("mismatched checksum", func(t *testing.T) {
		checksums := fmt.Sprintf("%s  archive.tar.gz\n", "0000000000000000000000000000000000000000000000000000000000000000")
		if err := verifyChecksum(data, "archive.tar.gz", []byte(checksums)); err == nil {
			t.Error("expected checksum mismatch error")
		}
	})

	t.Run("missing entry fails", func(t *testing.T) {
		checksums := "deadbeef  other.tar.gz\n"
		if err := verifyChecksum(data, "archive.tar.gz", []byte(checksums)); err == nil {
			t.Error("expected error for missing checksum entry")
		}
	})
}

func TestExtractBinaries(t *testing.T) {
	// Build a tar.gz with two files: "bin1" and "bin2".
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	files := map[string]string{
		"bin1":     "binary-one-content",
		"bin2":     "binary-two-content",
		"unwanted": "should-not-extract",
	}
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractBinaries(buf.Bytes(), dir, []string{"bin1", "bin2"}); err != nil {
		t.Fatalf("extractBinaries: %v", err)
	}

	// Verify extracted files.
	for _, name := range []string{"bin1", "bin2"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(data) != files[name] {
			t.Errorf("%s: got %q, want %q", name, data, files[name])
		}
	}

	// Verify unwanted file was not extracted.
	if _, err := os.Stat(filepath.Join(dir, "unwanted")); err == nil {
		t.Error("unwanted file should not have been extracted")
	}
}

func TestExtractBinariesMissing(t *testing.T) {
	// Archive with only "bin1" but we want "bin1" and "bin2".
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{Name: "bin1", Mode: 0755, Size: 3}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	err := extractBinaries(buf.Bytes(), dir, []string{"bin1", "bin2"})
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestExtractBinariesStripsDirectory(t *testing.T) {
	// Archive with a nested path: "subdir/mybinary".
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := "nested-content"
	hdr := &tar.Header{Name: "subdir/mybinary", Mode: 0755, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractBinaries(buf.Bytes(), dir, []string{"mybinary"}); err != nil {
		t.Fatalf("extractBinaries: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "mybinary"))
	if err != nil {
		t.Fatalf("read mybinary: %v", err)
	}
	if string(data) != content {
		t.Errorf("got %q, want %q", data, content)
	}
}

func TestResolveVersionPinned(t *testing.T) {
	dir := t.TempDir()
	versionFile := filepath.Join(dir, ".memory-version")
	if err := os.WriteFile(versionFile, []byte("1.2.3"), 0644); err != nil {
		t.Fatal(err)
	}

	version, err := resolveVersion(t.Context(), versionFile)
	if err != nil {
		t.Fatalf("resolveVersion: %v", err)
	}
	if version != "1.2.3" {
		t.Errorf("got %q, want %q", version, "1.2.3")
	}
}

func TestResolveVersionEnvOverride(t *testing.T) {
	t.Setenv("MEMORY_VERSION", "9.9.9")
	version, err := resolveVersion(t.Context(), "/nonexistent")
	if err != nil {
		t.Fatalf("resolveVersion: %v", err)
	}
	if version != "9.9.9" {
		t.Errorf("got %q, want %q", version, "9.9.9")
	}
}

// stubTransport serves canned bodies by URL suffix; unknown paths 404.
type stubTransport struct {
	bodies map[string][]byte
}

func (s stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for suffix, body := range s.bodies {
		if strings.HasSuffix(req.URL.Path, suffix) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(body)),
				Request:    req,
			}, nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("not found")),
		Request:    req,
	}, nil
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDownloadReleaseFailsClosedWithoutChecksums proves a missing
// checksums asset aborts the install instead of extracting unverified.
func TestDownloadReleaseFailsClosedWithoutChecksums(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	archive := tarGz(t, map[string]string{"bin1": "content"})
	sum := sha256.Sum256(archive)
	checksums := hex.EncodeToString(sum[:]) + "  a.tar.gz\n"

	orig := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = orig })

	httpClient.Transport = stubTransport{bodies: map[string][]byte{"/a.tar.gz": archive}}
	binDir := t.TempDir()
	if err := downloadRelease(context.Background(), "server/v1", "a.tar.gz", "checksums.txt", binDir, []string{"bin1"}); err == nil {
		t.Fatal("expected error when checksums asset is unavailable")
	}
	if _, err := os.Stat(filepath.Join(binDir, "bin1")); err == nil {
		t.Error("binary must not be extracted without checksum verification")
	}

	httpClient.Transport = stubTransport{bodies: map[string][]byte{"/a.tar.gz": archive, "/checksums.txt": []byte(checksums)}}
	if err := downloadRelease(context.Background(), "server/v1", "a.tar.gz", "checksums.txt", binDir, []string{"bin1"}); err != nil {
		t.Fatalf("downloadRelease with valid checksums: %v", err)
	}
	if _, err := os.Stat(filepath.Join(binDir, "bin1")); err != nil {
		t.Errorf("binary not extracted after verification: %v", err)
	}
}
