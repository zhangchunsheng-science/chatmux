// Package update implements chatmux self-update from GitHub releases —
// botmux 自更新's counterpart. Downloads the latest release asset for
// the running GOOS/GOARCH, extracts the binary, and swaps it in place.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zhangchunsheng-science/chatmux/internal/version"
)

const releasesAPI = "https://api.github.com/repos/zhangchunsheng-science/chatmux/releases/latest"

// Asset is one downloadable release file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// fetcher is swappable for tests.
type fetcher func(ctx context.Context, url string) ([]byte, error)

func httpFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "chatmux-selfupdate/"+version.Version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// FindAsset picks the release file for goos/goarch: tar.gz everywhere
// except windows, which ships zip.
func FindAsset(assets []Asset, goos, goarch string) (Asset, bool) {
	want := "chatmux-" + goos + "-" + goarch
	alt := ".tar.gz"
	if goos == "windows" {
		alt = ".zip"
	}
	for _, a := range assets {
		if a.Name == want+alt && a.URL != "" {
			return a, true
		}
	}
	return Asset{}, false
}

// ExtractBinary pulls the chatmux binary out of a tar.gz or zip asset.
func ExtractBinary(body []byte, name string) ([]byte, error) {
	switch {
	case strings.HasSuffix(name, ".tar.gz"):
		return extractTarGz(body)
	case strings.HasSuffix(name, ".zip"):
		return extractZip(body)
	}
	return nil, fmt.Errorf("unknown asset type: %s", name)
}

func extractTarGz(body []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("no chatmux binary in tarball")
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && strings.HasPrefix(filepath.Base(hdr.Name), "chatmux") {
			return io.ReadAll(io.LimitReader(tr, 128<<20))
		}
	}
}

func extractZip(body []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if strings.HasPrefix(filepath.Base(f.Name), "chatmux") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 128<<20))
		}
	}
	return nil, errors.New("no chatmux binary in zip")
}

// Result reports what Apply did.
type Result struct {
	UpToDate bool
	From     string
	To       string
}

// Apply checks the latest release and, if newer, replaces the running
// executable. Returns UpToDate when no action was needed.
func Apply(ctx context.Context) (Result, error) {
	exe, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return Result{}, err
	}
	return apply(ctx, httpFetch, releasesAPI, runtime.GOOS, runtime.GOARCH, version.Version, exe)
}

func apply(ctx context.Context, fetch fetcher, metaURL, goos, goarch, current, exe string) (Result, error) {
	body, err := fetch(ctx, metaURL)
	if err != nil {
		return Result{}, err
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return Result{}, err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	res := Result{From: current, To: latest}
	if latest == "" || latest == current {
		res.UpToDate = true
		return res, nil
	}
	asset, ok := FindAsset(rel.Assets, goos, goarch)
	if !ok {
		return res, fmt.Errorf("release %s has no asset for %s/%s", rel.TagName, goos, goarch)
	}
	archive, err := fetch(ctx, asset.URL)
	if err != nil {
		return res, err
	}
	bin, err := ExtractBinary(archive, asset.Name)
	if err != nil {
		return res, err
	}
	if err := swapBinary(exe, bin); err != nil {
		return res, err
	}
	return res, nil
}

// swapBinary replaces exe with bin via a temp file + rename (atomic on
// unix; the running process keeps its image until exit).
func swapBinary(exe string, bin []byte) error {
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
