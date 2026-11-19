package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
