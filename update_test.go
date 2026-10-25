package main

// update_test.go — 资产选择、版本比较、压缩包解压。不联网。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"windows", "amd64", "onet-windows-amd64.zip"},
		{"windows", "arm64", "onet-windows-arm64.zip"},
		{"darwin", "amd64", "onet-darwin-amd64.tar.gz"},
		{"darwin", "arm64", "onet-darwin-arm64.tar.gz"},
		{"linux", "amd64", "onet-linux-amd64.tar.gz"},
		{"linux", "arm64", "onet-linux-arm64.tar.gz"},
		{"linux", "arm", "onet-linux-arm-armv7.tar.gz"},
		{"freebsd", "amd64", "onet-freebsd-amd64.tar.gz"},
	}
	for _, c := range cases {
		got, err := assetName(c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("assetName(%s, %s) = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
	if _, err := assetName("linux", "ppc64le"); err == nil {
		t.Error("assetName(linux, ppc64le) 应返回错误")
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        int
		ok          bool
	}{
		{"v0.1.2", "0.1.1", 1, true},
		{"0.1.2", "v0.1.2", 0, true},
		{"0.1.9", "0.1.10", -1, true},
		{"0.1", "0.1.0", 0, true},
		{"0.1.2-rc1", "v0.1.2", 0, true},
		{"dev", "v0.1.2", 0, false},
	}
	for _, c := range cases {
		got, ok := compareVersion(c.cur, c.latest)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("compareVersion(%q, %q) = %d, %v; want %d, %v", c.cur, c.latest, got, ok, c.want, c.ok)
		}
	}
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "onet-windows-amd64.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	if err := writeZipEntry(zw, "onet-windows-amd64/", ""); err != nil {
		t.Fatal(err)
	}
	if err := writeZipEntry(zw, "onet-windows-amd64/README.md", "readme"); err != nil {
		t.Fatal(err)
	}
	if err := writeZipEntry(zw, "onet-windows-amd64/onet.exe", "fake-binary"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := extractBinary(archive, t.TempDir())
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	assertExtracted(t, out)
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "onet-linux-amd64.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	entries := []struct{ name, body string }{
		{"onet-linux-amd64/README.md", "readme"},
		{"onet-linux-amd64/LICENSE", "license"},
		{"onet-linux-amd64/onet", "fake-binary"},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := extractBinary(archive, t.TempDir())
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	assertExtracted(t, out)
}

func TestExtractBinaryMissing(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "empty.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinary(archive, t.TempDir()); err == nil {
		t.Error("压缩包里没有 onet 时应返回错误")
	}
}

func writeZipEntry(zw *zip.Writer, name, body string) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(body))
	return err
}

func assertExtracted(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "fake-binary" {
		t.Fatalf("内容 = %q, want %q", body, "fake-binary")
	}
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("权限 = %o, want 0755", perm)
	}
}
