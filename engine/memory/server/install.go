package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// Client release: demarkus.
	requiredBins = "demarkus-server,demarkus-token,demarkus"

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
func install(binDir, versionFile string) error {
	platform, arch, err := detectPlatform()
	if err != nil {
		return err
	}

	version, err := resolveVersion(versionFile)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}

	// Server release: demarkus-server + demarkus-token.
	slog.Info("memory install: downloading server", "version", version, "platform", platform, "arch", arch)
	if err := downloadRelease("server", version, platform, arch, binDir, []string{"demarkus-server", "demarkus-token"}); err != nil {
		return fmt.Errorf("server release: %w", err)
	}

	// Client release: demarkus CLI.
	clientVersion, err := fetchLatestVersion("client")
	if err != nil {
		return fmt.Errorf("fetch client version: %w", err)
	}
	slog.Info("memory install: downloading client", "version", clientVersion, "platform", platform, "arch", arch)
	if err := downloadRelease("client", clientVersion, platform, arch, binDir, []string{"demarkus"}); err != nil {
		return fmt.Errorf("client release: %w", err)
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
func resolveVersion(versionFile string) (string, error) {
	// Check env override first.
	if v := os.Getenv("MEMORY_VERSION"); v != "" {
		return v, nil
	}
	// Check pinned version file.
	if data, err := os.ReadFile(versionFile); err == nil {
		v := strings.TrimSpace(string(data))
		if v != "" {
			slog.Info("memory install: using pinned version", "version", v)
			return v, nil
		}
	}
	// Fetch latest.
	return fetchLatestVersion("server")
}

// fetchLatestVersion queries the GitHub releases API for the latest version
// of the given component ("server" or "client").
func fetchLatestVersion(component string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases", githubRepo)

	req, err := http.NewRequest("GET", url, nil)
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
	defer func() { _ = resp.Body.Close() }()

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

// downloadRelease fetches, verifies, and extracts specific binaries from a release.
func downloadRelease(component, version, platform, arch, binDir string, wantBins []string) error {
	tag := component + "/v" + version
	archiveName := fmt.Sprintf("demarkus-%s_%s_%s_%s.tar.gz", component, version, platform, arch)
	checksumsName := fmt.Sprintf("demarkus-%s_checksums.txt", component)

	// Download archive.
	archiveData, err := downloadAsset(tag, archiveName)
	if err != nil {
		return fmt.Errorf("download %s: %w", archiveName, err)
	}

	// Download and verify checksums.
	checksumsData, err := downloadAsset(tag, checksumsName)
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

// downloadAsset fetches a release asset from GitHub.
func downloadAsset(tag, filename string) ([]byte, error) {
	// For private repos with GITHUB_TOKEN, we'd need the asset API.
	// For public repos, direct download URL works.
	token := os.Getenv("GITHUB_TOKEN")

	if token != "" {
		return downloadAssetViaAPI(tag, filename, token)
	}

	encodedTag := strings.ReplaceAll(tag, "/", "%2F")
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", githubRepo, encodedTag, filename)

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, filename)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
}

// downloadAssetViaAPI uses the GitHub releases API to download assets from private repos.
func downloadAssetViaAPI(tag, filename, token string) ([]byte, error) {
	encodedTag := strings.ReplaceAll(tag, "/", "%2F")
	releaseURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", githubRepo, encodedTag)

	req, err := http.NewRequest("GET", releaseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

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
		assetReq, err := http.NewRequest("GET", asset.URL, nil)
		if err != nil {
			return nil, err
		}
		assetReq.Header.Set("Authorization", "token "+token)
		assetReq.Header.Set("Accept", "application/octet-stream")

		assetResp, err := httpClient.Do(assetReq)
		if err != nil {
			return nil, err
		}
		defer func() { _ = assetResp.Body.Close() }()

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

	slog.Warn("memory install: no checksum entry found, skipping verification", "file", archiveName)
	return nil
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
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	found := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		// Strip any directory prefix — we only care about the filename.
		name := filepath.Base(hdr.Name)
		if !want[name] {
			continue
		}

		dst := filepath.Join(binDir, name)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", name, err)
		}
		slog.Info("memory install: extracted", "binary", name, "path", dst)
		found++
	}

	if found < len(wantBins) {
		return fmt.Errorf("expected %d binaries, found %d in archive", len(wantBins), found)
	}
	return nil
}
