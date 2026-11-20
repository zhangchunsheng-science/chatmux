package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFindAsset(t *testing.T) {
	assets := []Asset{
		{Name: "chatmux-darwin-arm64.tar.gz", URL: "u1"},
		{Name: "chatmux-windows-amd64.zip", URL: "u2"},
		{Name: "checksums.txt", URL: "u3"},
	}
	if a, ok := FindAsset(assets, "darwin", "arm64"); !ok || a.URL != "u1" {
		t.Fatalf("darwin/arm64: %+v ok=%v", a, ok)
	}
	if a, ok := FindAsset(assets, "windows", "amd64"); !ok || a.URL != "u2" {
		t.Fatalf("windows/amd64: %+v ok=%v", a, ok)
	}
	if _, ok := FindAsset(assets, "linux", "s390x"); ok {
		t.Fatal("missing platform must not match")
	}
}

func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	got, err := ExtractBinary(tarGz(t, "chatmux-darwin-arm64", "MACHO"), "chatmux-darwin-arm64.tar.gz")
	if err != nil || string(got) != "MACHO" {
		t.Fatalf("tar.gz: %q err=%v", got, err)
	}
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("chatmux-windows-amd64.exe")
	w.Write([]byte("MZ"))
	zw.Close()
	got, err = ExtractBinary(zbuf.Bytes(), "chatmux-windows-amd64.zip")
	if err != nil || string(got) != "MZ" {
		t.Fatalf("zip: %q err=%v", got, err)
	}
	if _, err := ExtractBinary([]byte("junk"), "chatmux.bin"); err == nil {
		t.Fatal("unknown type should error")
	}
}

func TestApplyUpToDate(t *testing.T) {
	res, err := apply(context.Background(),
		func(ctx context.Context, url string) ([]byte, error) {
			return []byte(`{"tag_name":"v0.0.1","assets":[]}`), nil
		}, "http://x", "linux", "amd64", "0.0.1", "/tmp/never-touched")
	if err != nil || !res.UpToDate || res.To != "0.0.1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestApplySwapsTempExe(t *testing.T) {
	assetName := "chatmux-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/meta":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets":   []Asset{{Name: assetName, URL: "http://" + r.Host + "/dl"}},
			})
		case "/dl":
			w.Write(tarGz(t, "chatmux-bin", "NEWBIN"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	exe := filepath.Join(t.TempDir(), "fake-chatmux")
	if err := os.WriteFile(exe, []byte("OLDBIN"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := apply(context.Background(), func(ctx context.Context, url string) ([]byte, error) {
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return buf.Bytes(), nil
	}, ts.URL+"/meta", runtime.GOOS, runtime.GOARCH, "0.0.1", exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.UpToDate || res.From != "0.0.1" || res.To != "9.9.9" {
		t.Fatalf("res=%+v", res)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "NEWBIN" {
		t.Fatalf("exe content %q want NEWBIN", got)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("temp .new file left behind")
	}
}

// applyServer serves a fake release: meta at /meta, archive at /dl,
// optional sidecar at /dl.sha256.
func applyServer(t *testing.T, tag string, archive []byte, sidecar *string) *httptest.Server {
	t.Helper()
	assetName := "chatmux-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	var assets []Asset
	if sidecar != nil {
		assets = append(assets,
			Asset{Name: assetName + ".sha256", URL: "sidecar"}, // rewritten to absolute in handler
		)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/meta":
			base := "http://" + r.Host
			as := append([]Asset{{Name: assetName, URL: base + "/dl"}}, assets...)
			for i := range as {
				if as[i].URL == "sidecar" {
					as[i].URL = base + "/dl.sha256"
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": as})
		case "/dl":
			w.Write(archive)
		case "/dl.sha256":
			if sidecar == nil {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(*sidecar))
		default:
			http.NotFound(w, r)
		}
	}))
}

func httpFetcher(ctx context.Context, url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return buf.Bytes(), nil
}

func TestApplyChecksumMatch(t *testing.T) {
	archive := tarGz(t, "chatmux-bin", "VERIFIED")
	sum := sha256.Sum256(archive)
	sidecar := fmt.Sprintf("%x  chatmux.tar.gz\n", sum)
	ts := applyServer(t, "v9.9.9", archive, &sidecar)
	defer ts.Close()

	exe := filepath.Join(t.TempDir(), "fake-chatmux")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	if _, err := apply(context.Background(), httpFetcher, ts.URL+"/meta", runtime.GOOS, runtime.GOARCH, "0.0.1", exe); err != nil {
		t.Fatalf("valid checksum rejected: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "VERIFIED" {
		t.Fatalf("exe %q", got)
	}
}

func TestApplyChecksumMismatchRefuses(t *testing.T) {
	archive := tarGz(t, "chatmux-bin", "TAMPERED")
	sidecar := "0000000000000000000000000000000000000000000000000000000000000000  chatmux.tar.gz\n"
	ts := applyServer(t, "v9.9.9", archive, &sidecar)
	defer ts.Close()

	exe := filepath.Join(t.TempDir(), "fake-chatmux")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	_, err := apply(context.Background(), httpFetcher, ts.URL+"/meta", runtime.GOOS, runtime.GOARCH, "0.0.1", exe)
	if err == nil || !strings.Contains(err.Error(), "refusing bad download") {
		t.Fatalf("mismatched checksum must refuse, got %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "OLD" {
		t.Fatalf("exe was replaced despite mismatch: %q", got)
	}
}

func TestApplyNoSidecarSkipsCheck(t *testing.T) {
	// releases before checksum publishing have no sidecar: update works
	archive := tarGz(t, "chatmux-bin", "LEGACY")
	ts := applyServer(t, "v9.9.9", archive, nil)
	defer ts.Close()

	exe := filepath.Join(t.TempDir(), "fake-chatmux")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	if _, err := apply(context.Background(), httpFetcher, ts.URL+"/meta", runtime.GOOS, runtime.GOARCH, "0.0.1", exe); err != nil {
		t.Fatalf("legacy release without sidecar must still update: %v", err)
	}
}
