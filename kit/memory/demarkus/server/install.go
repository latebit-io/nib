package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	githubRepo = "latebit-io/demarkus"
	// requiredBins lists the binaries that must be present for memory to work.
	// Server release: demarkus-server, demarkus-token.
	// Client release: demarkus, demarkus-mcp.
	requiredBins = "demarkus-server,demarkus-token,demarkus,demarkus-mcp"

	// httpTimeout bounds all GitHub API and download requests.
	httpTimeout = 60 * time.Second
	// maxDownloadBytes caps release asset downloads (100 MB).
	maxDownloadBytes = 100 << 20
	// maxAPIResponseBytes caps GitHub API JSON responses (2 MB).
	maxAPIResponseBytes = 2 << 20
)

// httpClient is a dedicated client with a timeout for all install-time HTTP requests.
var httpClient = &http.Client{Timeout: httpTimeout}

// install downloads and installs the demarkus binaries into binDir.
// version is the pinned version to install (empty means fetch latest).
// ctx bounds all network I/O so a stalled download can be cancelled.
func install(ctx context.Context, binDir, versionFile string) error {
	platform, arch, err := detectPlatform()
	if err != nil {
		return err
	}

	version, err := resolveVersion(ctx, versionFile)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}

	// Server release: demarkus-server + demarkus-token in one archive.
	slog.Info("memory install: downloading server", "version", version, "platform", platform, "arch", arch)
	serverTag := "server/v" + version
	serverArchive := fmt.Sprintf("demarkus-server_%s_%s_%s.tar.gz", version, platform, arch)
	if err := downloadRelease(ctx, serverTag, serverArchive, "demarkus-server_checksums.txt", binDir, []string{"demarkus-server", "demarkus-token"}); err != nil {
		return fmt.Errorf("server release: %w", err)
	}

	// Client release: demarkus CLI and demarkus-mcp live in separate archives
	// under a single release tag and share one checksums file.
	clientVersion, err := fetchLatestVersion(ctx, "client")
	if err != nil {
		return fmt.Errorf("fetch client version: %w", err)
	}
	slog.Info("memory install: downloading client", "version", clientVersion, "platform", platform, "arch", arch)
	clientTag := "client/v" + clientVersion
	clientChecksums := "demarkus-client_checksums.txt"
	clientArchive := fmt.Sprintf("demarkus-client_%s_%s_%s.tar.gz", clientVersion, platform, arch)
	if err := downloadRelease(ctx, clientTag, clientArchive, clientChecksums, binDir, []string{"demarkus"}); err != nil {
		return fmt.Errorf("client release: %w", err)
	}
	mcpArchive := fmt.Sprintf("demarkus-mcp_%s_%s_%s.tar.gz", clientVersion, platform, arch)
	if err := downloadRelease(ctx, clientTag, mcpArchive, clientChecksums, binDir, []string{"demarkus-mcp"}); err != nil {
		return fmt.Errorf("mcp release: %w", err)
	}

	// Pin version.
	if err := os.WriteFile(versionFile, []byte(version), 0644); err != nil {
		return fmt.Errorf("write version file: %w", err)
	}

	slog.Info("memory install: complete", "binDir", binDir, "version", version)
	return nil
}

// detectPlatform returns the GOOS and GOARCH values normalized to demarkus release naming.
func detectPlatform() (platform, arch string, err error) {
	switch runtime.GOOS {
	case "darwin", "linux":
		platform = runtime.GOOS
	default:
		return "", "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		arch = runtime.GOARCH
	default:
		return "", "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
	return platform, arch, nil
}

// resolveVersion reads the pinned version from versionFile, or fetches latest from GitHub.
func resolveVersion(ctx context.Context, versionFile string) (string, error) {
	// Check env override first.
	if v := os.Getenv("MEMORY_VERSION"); v != "" {
		return v, nil
	}
	// Check pinned version file. Only fall back to latest if the file
	// doesn't exist — permission or I/O errors must abort, not silently upgrade.
	data, err := os.ReadFile(versionFile)
	if err == nil {
		v := strings.TrimSpace(string(data))
		if v != "" {
			slog.Info("memory install: using pinned version", "version", v)
			return v, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read pinned version: %w", err)
	}
	// Fetch latest.
	return fetchLatestVersion(ctx, "server")
}

// fetchLatestVersion queries the GitHub releases API for the latest version
// of the given component ("server" or "client"). ctx bounds the request.
func fetchLatestVersion(ctx context.Context, component string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases", githubRepo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort; response already consumed

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&releases); err != nil {
		return "", fmt.Errorf("decode releases: %w", err)
	}

	prefix := component + "/v"
	for _, r := range releases {
		if strings.HasPrefix(r.TagName, prefix) {
			return strings.TrimPrefix(r.TagName, prefix), nil
		}
	}
	return "", fmt.Errorf("no %s release found", component)
}

// downloadRelease fetches, verifies, and extracts specific binaries from a
// release asset. tag is the git tag (e.g. "client/v1.2.3"). archiveName and
// checksumsName are the asset filenames; multiple archives in the same release
// may share a single checksums file. ctx bounds all network I/O.
func downloadRelease(ctx context.Context, tag, archiveName, checksumsName, binDir string, wantBins []string) error {
	// Download archive.
	archiveData, err := downloadAsset(ctx, tag, archiveName)
	if err != nil {
		return fmt.Errorf("download %s: %w", archiveName, err)
	}

	// Download and verify checksums.
	checksumsData, err := downloadAsset(ctx, tag, checksumsName)
	if err != nil {
		slog.Warn("memory install: checksums unavailable, skipping verification", "err", err)
	} else {
		if err := verifyChecksum(archiveData, archiveName, checksumsData); err != nil {
			return err
		}
	}

	// Extract wanted binaries from the archive.
	return extractBinaries(archiveData, binDir, wantBins)
}

// downloadAsset fetches a release asset from GitHub. ctx bounds the request.
func downloadAsset(ctx context.Context, tag, filename string) ([]byte, error) {
	// For private repos with GITHUB_TOKEN, we'd need the asset API.
	// For public repos, direct download URL works.
	token := os.Getenv("GITHUB_TOKEN")

	if token != "" {
		return downloadAssetViaAPI(ctx, tag, filename, token)
	}

	encodedTag := strings.ReplaceAll(tag, "/", "%2F")
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", githubRepo, encodedTag, filename)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // best-effort; response already consumed

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, filename)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
}

// downloadAssetViaAPI uses the GitHub releases API to download assets from private repos.
func downloadAssetViaAPI(ctx context.Context, tag, filename, token string) ([]byte, error) {
	encodedTag := strings.ReplaceAll(tag, "/", "%2F")
	releaseURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", githubRepo, encodedTag)

	req, err := http.NewRequestWithContext(ctx, "GET", releaseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // best-effort; response already consumed

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release API returned %d for tag %s", resp.StatusCode, tag)
	}

	var release struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}

	for _, asset := range release.Assets {
		if asset.Name != filename {
			continue
		}
		assetReq, err := http.NewRequestWithContext(ctx, "GET", asset.URL, nil)
		if err != nil {
			return nil, err
		}
		assetReq.Header.Set("Authorization", "token "+token)
		assetReq.Header.Set("Accept", "application/octet-stream")

		assetResp, err := httpClient.Do(assetReq)
		if err != nil {
			return nil, err
		}
		defer func() { _ = assetResp.Body.Close() }() // best-effort; response already consumed

		if assetResp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("asset download returned %d", assetResp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(assetResp.Body, maxDownloadBytes))
	}
	return nil, fmt.Errorf("asset %s not found in release %s", filename, tag)
}

// verifyChecksum checks the archive data against the checksums file.
func verifyChecksum(archiveData []byte, archiveName string, checksumsData []byte) error {
	sum := sha256.Sum256(archiveData)
	actual := hex.EncodeToString(sum[:])

	for line := range strings.Lines(string(checksumsData)) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == archiveName {
			if fields[0] != actual {
				return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", archiveName, fields[0], actual)
			}
			slog.Info("memory install: checksum verified", "file", archiveName)
			return nil
		}
	}

	return fmt.Errorf("checksum entry for %s not found in checksums file", archiveName)
}

// extractBinaries extracts named binaries from a tar.gz archive into binDir.
func extractBinaries(archiveData []byte, binDir string, wantBins []string) error {
	want := make(map[string]bool, len(wantBins))
	for _, name := range wantBins {
		want[name] = true
	}

	gr, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gr.Close() }() // best-effort; data already extracted

	tr := tar.NewReader(gr)
	extracted := make(map[string]bool, len(wantBins))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		// Strip any directory prefix — we only care about the filename.
		name := filepath.Base(hdr.Name)
		if !want[name] || extracted[name] {
			continue
		}

		dst := filepath.Join(binDir, name)

		// Write to a temp file first, then rename into place.
		// This avoids truncating an existing binary if the copy fails.
		tmp, err := os.CreateTemp(binDir, name+".tmp.*")
		if err != nil {
			return fmt.Errorf("create temp for %s: %w", name, err)
		}
		tmpPath := tmp.Name()

		// Error paths below do best-effort cleanup of the temp file.
		// Close/Remove errors are safe to ignore — the primary error is returned.
		if _, err := io.Copy(tmp, tr); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("write %s: %w", name, err)
		}
		if err := tmp.Chmod(0755); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("chmod %s: %w", name, err)
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("close %s: %w", name, err)
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("rename %s: %w", name, err)
		}
		slog.Info("memory install: extracted", "binary", name, "path", dst)
		extracted[name] = true
	}

	if len(extracted) < len(wantBins) {
		return fmt.Errorf("expected %d binaries, found %d in archive", len(wantBins), len(extracted))
	}
	return nil
}
