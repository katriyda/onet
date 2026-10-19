package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写入 count 行固定前缀记录，便于断言轮转后的留存与淘汰。
func writeLogLines(t *testing.T, w *rotatingWriter, count int) {
	t.Helper()
	for i := range count {
		line := fmt.Sprintf("line-%02d %s\n", i, strings.Repeat("x", 28))
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("写入第 %d 行失败: %v", i, err)
		}
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

func TestRotatingWriterRotatesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onet.log")

	w, err := newRotatingWriter(path, 120, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	writeLogLines(t, w, 20)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("%s.1 应存在: %v", path, err)
	}

	// 当前文件 + 2 份历史，最多 3 个。
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(files) > 3 {
		t.Fatalf("轮转后文件数应 ≤3，实际 %d: %v", len(files), files)
	}

	all := ""
	for _, f := range files {
		all += readFileString(t, f)
	}
	if !strings.Contains(all, "line-19") {
		t.Fatalf("最新记录 line-19 应保留，实际内容:\n%s", all)
	}
	if strings.Contains(all, "line-00") {
		t.Fatalf("最旧记录 line-00 应已被删除，实际内容:\n%s", all)
	}
}

func TestTailFileReturnsLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onet.log")
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "line-%02d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := tailFile(path, 3, 1<<20)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	want := "line-08\nline-09\nline-10\n"
	if string(got) != want {
		t.Fatalf("tailFile = %q，期望 %q", got, want)
	}
}

func TestTailFileDropsPartialFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onet.log")
	var sb strings.Builder
	for i := range 20 {
		fmt.Fprintf(&sb, "line-%02d %s\n", i, strings.Repeat("x", 40))
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	const maxBytes = 100

	got, err := tailFile(path, 10, maxBytes)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("tailFile 返回空，应至少包含一条完整记录")
	}
	first := strings.SplitN(string(got), "\n", 2)[0]
	if !strings.HasPrefix(first, "line-") || !strings.HasSuffix(first, strings.Repeat("x", 40)) {
		t.Fatalf("首行应为完整记录，实际 %q", first)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Fatalf("返回值应以换行结尾，实际 %q", got)
	}
	// 尾部记录必须是最新的那条。
	if !strings.Contains(string(got), "line-19 ") {
		t.Fatalf("应包含最新记录 line-19，实际:\n%s", got)
	}
	if strings.Contains(string(got), "line-00 ") {
		t.Fatalf("不应包含超出窗口的旧记录，实际:\n%s", got)
	}
}

func TestRedactedQueryMasksToken(t *testing.T) {
	got := redactedQuery("token=abc&s=foo")
	if got == "<invalid>" {
		t.Fatal("合法查询串不应判为 invalid")
	}
	q, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", got, err)
	}
	if v := q["token"]; len(v) != 1 || v[0] != "***" {
		t.Fatalf("token 应脱敏为 ***，实际 %q", v)
	}
	if v := q["s"]; len(v) != 1 || v[0] != "foo" {
		t.Fatalf("非敏感字段应保留，实际 %q", v)
	}
	if strings.Contains(got, "abc") {
		t.Fatalf("脱敏结果泄露了 token: %q", got)
	}
}

func TestParseLogLevel(t *testing.T) {
	for _, raw := range []string{"debug", "info", "warn", "error", "ERROR", " Info "} {
		if _, err := parseLogLevel(raw); err != nil {
			t.Fatalf("parseLogLevel(%q) 不应报错: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "trace"} {
		if _, err := parseLogLevel(raw); err == nil {
			t.Fatalf("parseLogLevel(%q) 应报错", raw)
		}
	}
}
