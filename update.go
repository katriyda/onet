package main

// update.go — `onet update`：查询 GitHub 最新 Release、按平台选资产、下载解压并原子替换当前二进制。
// 只替换文件，不重启服务、不动会话；运行中的进程继续用旧 inode，重启后生效。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultReleaseAPI = "https://api.github.com/repos/katriyda/onet/releases/latest"
	maxAssetBytes     = 64 << 20
)

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type releaseInfo struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

// cleanupStaleBinary 删除上一次更新留下的 *.old 备份。
// 旧服务进程仍在运行时 Windows 上删不掉，属正常情况：所有错误一律忽略。
func cleanupStaleBinary() {
	exe, err := resolveExe()
	if err != nil {
		return
	}
	_ = os.Remove(exe + ".old")
	stale, err := filepath.Glob(exe + ".*.old")
	if err != nil {
		return
	}
	for _, p := range stale {
		_ = os.Remove(p)
	}
}

// resolveExe 返回当前二进制的真实路径（跟随符号链接，保证替换真实文件而不是链接）。
func resolveExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return real, nil
}

// releaseEndpoint 允许用 ONET_RELEASE_API 指向镜像/测试服务。
func releaseEndpoint() string {
	if v := os.Getenv("ONET_RELEASE_API"); v != "" {
		return v
	}
	return defaultReleaseAPI
}

// fetchLatest 查询最新 Release。GITHUB_TOKEN / GH_TOKEN 存在时用于提高 API 限额。
func fetchLatest(ctx context.Context, client *http.Client) (releaseInfo, error) {
	var rel releaseInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseEndpoint(), nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("User-Agent", "onet-updater/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if tok := os.Getenv("GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return rel, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return rel, fmt.Errorf("GitHub API 返回 %s: %s", resp.Status, oneLine(body))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return rel, fmt.Errorf("解析 Release 响应失败: %w", err)
	}
	return rel, nil
}

// oneLine 把响应体压成单行，避免错误信息里夹带换行。
func oneLine(b []byte) string {
	return strings.Join(strings.Fields(string(b)), " ")
}

// assetName 按平台选资产名；必须与 release.yml 的 matrix 一致，新增平台时同步改这里。
func assetName(goos, goarch string) (string, error) {
	switch goos {
	case "windows":
		if goarch == "amd64" || goarch == "arm64" {
			return "onet-windows-" + goarch + ".zip", nil
		}
	case "darwin":
		if goarch == "amd64" || goarch == "arm64" {
			return "onet-darwin-" + goarch + ".tar.gz", nil
		}
	case "linux":
		switch goarch {
		case "arm":
			// 运行时拿不到 GOARM，发布版只有 armv7，一律按 v7 处理。
			return "onet-linux-arm-armv7.tar.gz", nil
		case "amd64", "arm64":
			return "onet-linux-" + goarch + ".tar.gz", nil
		}
	case "freebsd":
		if goarch == "amd64" {
			return "onet-freebsd-amd64.tar.gz", nil
		}
	}
	return "", fmt.Errorf("不支持的平台 %s/%s", goos, goarch)
}

// compareVersion 比较两个版本号（忽略 v/V 前缀与 -pre 后缀），返回 cur 相对 latest 的 -1/0/1。
// ok=false 表示有版本号不是纯数字，无法比较（如源码构建的 dev）。
func compareVersion(cur, latest string) (int, bool) {
	a, ok := parseVersion(cur)
	if !ok {
		return 0, false
	}
	b, ok := parseVersion(latest)
	if !ok {
		return 0, false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func parseVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil, false
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// updateCandidate 是当前平台可安装的最新发布版。
type updateCandidate struct {
	Rel        releaseInfo
	Asset      releaseAsset
	Cmp        int  // current 相对 latest 的 -1/0/1
	Comparable bool // false = 当前为 dev 等无法比较的版本
}

// upToDate 判断是否无需安装。force 允许降级/重装。
func (c updateCandidate) upToDate(force bool) bool {
	if !c.Comparable {
		return false
	}
	return c.Cmp >= 0 && !force
}

// findCandidate 查询最新 Release 并选出当前平台的资产。
func findCandidate(ctx context.Context, client *http.Client) (updateCandidate, error) {
	rel, err := fetchLatest(ctx, client)
	if err != nil {
		return updateCandidate{}, err
	}
	name, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return updateCandidate{}, err
	}
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			cmp, ok := compareVersion(version, rel.Tag)
			return updateCandidate{Rel: rel, Asset: rel.Assets[i], Cmp: cmp, Comparable: ok}, nil
		}
	}
	return updateCandidate{}, fmt.Errorf("发布版 %s 中没有 %s", rel.Tag, name)
}

// normalizeVersion 去掉 v 前缀与空白，便于比较子进程输出的版本号。
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "V")
}

// validateBinary 在安装前运行 `onet version`，避免把无法启动的二进制换上去。
func validateBinary(ctx context.Context, path, want string) error {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("新版本二进制无法运行: %v: %s", err, oneLine(out))
	}
	got := strings.TrimSpace(string(out))
	if normalizeVersion(got) != normalizeVersion(want) {
		return fmt.Errorf("新二进制版本 %q 与发布版 %q 不匹配", got, want)
	}
	return nil
}

// installCandidate 下载、解压、校验并原子替换当前二进制。返回被替换的路径。
func installCandidate(ctx context.Context, client *http.Client, cand updateCandidate) (string, error) {
	exe, err := resolveExe()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	archive, err := downloadAsset(ctx, client, cand.Asset.URL, dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(archive) }()
	newPath, err := extractBinary(archive, dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(newPath) }()
	if err := validateBinary(ctx, newPath, cand.Rel.Tag); err != nil {
		return "", err
	}
	if err := installBinary(exe, newPath); err != nil {
		return "", err
	}
	return exe, nil
}

// downloadAsset 把资产下载到 dir 下的临时文件；保留原扩展名，供 extractBinary 判类型。
func downloadAsset(ctx context.Context, client *http.Client, url, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "onet-updater/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 %s 失败: %s", url, resp.Status)
	}
	base := url
	if i := strings.IndexByte(base, '?'); i >= 0 {
		base = base[:i]
	}
	f, err := os.CreateTemp(dir, ".onet-download-*"+path.Ext(base))
	if err != nil {
		return "", err
	}
	written, err := io.Copy(f, io.LimitReader(resp.Body, maxAssetBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	if written > maxAssetBytes {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("资源超过 %d 字节上限", maxAssetBytes)
	}
	return f.Name(), nil
}

// newBinaryPattern 临时文件保留可执行扩展名，Windows 上才能直接运行做版本预检。
func newBinaryPattern() string {
	if runtime.GOOS == "windows" {
		return ".onet-new-*.exe"
	}
	return ".onet-new-*"
}

// extractBinary 从 zip / tar.gz 中取出 onet 二进制，写到 dir 下的临时文件并返回其路径。
func extractBinary(archivePath, dir string) (string, error) {
	out, err := os.CreateTemp(dir, newBinaryPattern())
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		err = copyZipBinary(archivePath, out)
	} else {
		err = copyTarGzBinary(archivePath, out)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	if err := os.Chmod(out.Name(), exePerm()); err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

func copyZipBinary(archivePath string, out *os.File) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isBinaryName(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		_, cerr := io.Copy(out, rc)
		if err := rc.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return cerr
		}
		return nil
	}
	return fmt.Errorf("压缩包中没有 onet 二进制")
}

func copyTarGzBinary(archivePath string, out *os.File) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("压缩包中没有 onet 二进制")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || !isBinaryName(hdr.Name) {
			continue
		}
		_, err = io.Copy(out, tr)
		return err
	}
}

func isBinaryName(name string) bool {
	base := path.Base(name)
	return base == "onet" || base == "onet.exe"
}

// exePerm 沿用当前二进制的权限位；拿不到或原文件没有执行位时退化为 0755。
func exePerm() os.FileMode {
	exe, err := resolveExe()
	if err != nil {
		return 0o755
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return 0o755
	}
	perm := fi.Mode().Perm()
	if perm&0o111 == 0 {
		return 0o755
	}
	return perm
}

// installBinary 用 newPath 替换 exe。Windows 上运行中的镜像不能覆盖但可以改名，
// 所以先改名旧文件再落新文件；任何一步失败都尽力回滚，保证当前二进制可用。
func installBinary(exe, newPath string) error {
	if runtime.GOOS != "windows" {
		// 同目录 rename 是原子的；运行中的进程继续持有旧 inode。
		return os.Rename(newPath, exe)
	}
	old := exe + ".old"
	if err := os.Rename(exe, old); err != nil {
		old = fmt.Sprintf("%s.%d.old", exe, time.Now().UnixNano())
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("无法移动正在使用的二进制，请退出 onet 后重试: %w", err)
		}
	}
	if err := os.Rename(newPath, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("替换失败: %v（回滚也失败: %v，原二进制在 %s）", err, rerr, old)
		}
		return fmt.Errorf("替换失败: %w", err)
	}
	return nil
}

func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	check := fs.Bool("check", false, "只检查是否有新版本，不下载安装")
	force := fs.Bool("force", false, "已是最新版本时也强制重新安装")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "update: 未知参数 %q\n", fs.Args())
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "更新失败: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Minute}

	cand, err := findCandidate(ctx, client)
	if err != nil {
		return fail(err)
	}
	rel := cand.Rel

	fmt.Printf("当前版本: %s\n", version)
	fmt.Printf("最新版本: %s\n", rel.Tag)
	fmt.Printf("目标资源: %s\n", cand.Asset.Name)

	switch {
	case !cand.Comparable:
		fmt.Printf("当前为开发版本 %q，将安装最新发布版\n", version)
	case cand.Cmp > 0:
		fmt.Printf("当前版本 %s 高于最新发布 %s", version, rel.Tag)
		if !*force {
			fmt.Println("，使用 -force 强制安装")
			return 0
		}
		fmt.Println("，按 -force 安装")
	case cand.Cmp == 0 && !*force:
		fmt.Println("已是最新版本")
		return 0
	default:
		fmt.Println("可更新")
	}
	if *check {
		return 0
	}

	fmt.Printf("正在下载 %s ...\n", cand.Asset.Name)
	exe, err := installCandidate(ctx, client, cand)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("已更新到 %s: %s\n", rel.Tag, exe)
	fmt.Println("运行中的 onet 仍在使用旧版本；网页「检查更新」可直接完成重启与切换")
	return 0
}
