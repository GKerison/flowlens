//go:build darwin

package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kerison/flowlens/internal/ca"
)

// newTestManager 用真实的 ca 包生成一张根证书，作为信任管理器的输入。
// 走真实生成流程而不是硬编码 PEM：这样证书解析、指纹计算、路径处理都被覆盖到。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	caMgr, err := ca.NewManager(dir)
	if err != nil {
		t.Fatalf("生成测试 CA 失败: %v", err)
	}
	m, err := New(caMgr.CertPath())
	if err != nil {
		t.Fatalf("创建 trust.Manager 失败: %v", err)
	}
	return m
}

func TestNewRejectsMissingFile(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "nope.pem")); err == nil {
		t.Fatal("证书文件不存在时应返回错误")
	}
}

func TestNewRejectsNonPEM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(p, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(p); err == nil {
		t.Fatal("非 PEM 内容应返回错误")
	}
}

func TestFingerprints(t *testing.T) {
	m := newTestManager(t)

	// SHA-256 以冒号分隔，共 32 组
	if got := strings.Count(m.SHA256(), ":"); got != 31 {
		t.Fatalf("SHA-256 指纹应为 32 组，实际分隔符 %d 个：%s", got, m.SHA256())
	}
	// SHA-1 为 40 位大写十六进制，certutil 与 security -Z 都按这个格式匹配
	sha1 := m.SHA1()
	if len(sha1) != 40 {
		t.Fatalf("SHA-1 指纹应为 40 字符，实际 %d：%s", len(sha1), sha1)
	}
	if sha1 != strings.ToUpper(sha1) {
		t.Fatalf("SHA-1 指纹应为大写：%s", sha1)
	}
	if m.CommonName() == "" {
		t.Fatal("根证书应有 CommonName")
	}
}

func TestInstallCommandShapes(t *testing.T) {
	m := newTestManager(t)

	user := installCommand(m, false)
	if user[0] != "security" || !contains(user, "add-trusted-cert") {
		t.Fatalf("用户级安装命令不符合预期: %v", user)
	}
	if contains(user, "sudo") {
		t.Fatalf("用户级安装不应带 sudo: %v", user)
	}
	if !contains(user, m.CertPath()) {
		t.Fatalf("安装命令应包含证书路径: %v", user)
	}

	sys := installCommand(m, true)
	if sys[0] != "sudo" {
		t.Fatalf("系统级安装应展示 sudo 前缀: %v", sys)
	}
	if !contains(sys, "-d") || !contains(sys, systemKeychain) {
		t.Fatalf("系统级安装应写入系统钥匙串: %v", sys)
	}
}

func TestStatusRunsAndReportsStore(t *testing.T) {
	m := newTestManager(t)
	st := m.Status()

	if st.Store.Name == "" {
		t.Fatal("应给出安装目标描述")
	}
	if st.Fingerprint == "" {
		t.Fatal("应返回指纹")
	}
	if len(st.Command) == 0 {
		t.Fatal("应返回预览命令")
	}
	if !st.CanElevate {
		t.Fatal("macOS 应支持提权安装")
	}
	// 只校验"未信任时提示文案有内容"，不假设本机当前状态
	if st.Detail == "" {
		t.Fatal("应给出状态说明")
	}
}

func TestShellAndAppleScriptQuoting(t *testing.T) {
	// 路径含空格是 macOS 的常态（Application Support），必须被引号包住
	got := shQuote("/a b/c.pem")
	if got != "'/a b/c.pem'" {
		t.Fatalf("shQuote 结果不对: %s", got)
	}
	// 单引号需要转义成 '\'' 才能安全嵌入
	got = shQuote("it's")
	if got != `'it'\''s'` {
		t.Fatalf("shQuote 未正确转义单引号: %s", got)
	}

	as := asQuote(`say "hi" \ there`)
	if !strings.HasPrefix(as, `"`) || !strings.HasSuffix(as, `"`) {
		t.Fatalf("asQuote 应加双引号: %s", as)
	}
	if !strings.Contains(as, `\"hi\"`) {
		t.Fatalf("asQuote 应转义内部双引号: %s", as)
	}
	if !strings.Contains(as, `\\`) {
		t.Fatalf("asQuote 应转义反斜杠: %s", as)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want || strings.Contains(s, want) {
			return true
		}
	}
	return false
}
