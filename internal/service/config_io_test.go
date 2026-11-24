package service_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kerison/flowlens/internal/config"
)

// TestConfigExportImportRoundTrip 覆盖 F9-3：导出 → 修改 → 导入 → 配置恢复。
func TestConfigExportImportRoundTrip(t *testing.T) {
	app, cfgMgr, _ := newTestAppWithStore(t)

	// 先把配置改成一组可辨认的值
	if err := cfgMgr.Update(func(c *config.Config) error {
		c.SkipDecryptHosts = []string{"secret.bank.com"}
		c.InterceptTLS = false
		c.MaxFlows = 3000
		return nil
	}); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	// 导出
	path := filepath.Join(t.TempDir(), "backup")
	res, err := app.ExportConfig(path)
	if err != nil {
		t.Fatalf("导出配置失败: %v", err)
	}
	if res.Path != path+".json" {
		t.Fatalf("应补 .json 扩展名，实际 %s", res.Path)
	}

	// 改掉配置，模拟"换个环境跑了一阵"
	if err := cfgMgr.Update(func(c *config.Config) error {
		c.SkipDecryptHosts = []string{}
		c.InterceptTLS = true
		c.MaxFlows = 5000
		return nil
	}); err != nil {
		t.Fatalf("改写配置失败: %v", err)
	}

	// 导入 → 恢复
	saved, err := app.ImportConfig(res.Path)
	if err != nil {
		t.Fatalf("导入配置失败: %v", err)
	}
	if saved.MaxFlows != 3000 || saved.InterceptTLS {
		t.Fatalf("导入后配置未恢复：maxFlows=%d interceptTLS=%v", saved.MaxFlows, saved.InterceptTLS)
	}
	if len(saved.SkipDecryptHosts) != 1 || saved.SkipDecryptHosts[0] != "secret.bank.com" {
		t.Fatalf("导入后跳过解密列表未恢复: %v", saved.SkipDecryptHosts)
	}
}

// TestImportAcceptsBareConfig 验证裸配置 JSON（无包装）也能导入，方便手工编辑。
func TestImportAcceptsBareConfig(t *testing.T) {
	app, _, _ := newTestAppWithStore(t)

	raw, err := json.Marshal(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	saved, err := app.ImportConfig(path)
	if err != nil {
		t.Fatalf("裸配置导入失败: %v", err)
	}
	if saved.MaxFlows != 5000 {
		t.Fatalf("导入后应为默认值 5000，实际 %d", saved.MaxFlows)
	}
}

// TestImportRejectsBadFiles 验证导入的防线：目录、坏 JSON、非法规则都要拦下。
func TestImportRejectsBadFiles(t *testing.T) {
	app, _, _ := newTestAppWithStore(t)

	dir := t.TempDir()
	if _, err := app.ImportConfig(dir); err == nil {
		t.Fatal("导入目录应报错")
	}

	bad := filepath.Join(dir, "broken.json")
	_ = os.WriteFile(bad, []byte("not json at all"), 0o644)
	if _, err := app.ImportConfig(bad); err == nil {
		t.Fatal("坏 JSON 应报错")
	}

	// 结构合法但 Map Remote 目标写错：必须拒绝而不是静默禁用
	badRule := `{"flowlens.config":"flowlens.config/1","config":{"mapRemoteRules":[{"enabled":true,"pattern":"x","target":"no-scheme"}]}}`
	wrapped := filepath.Join(dir, "wrapped.json")
	_ = os.WriteFile(wrapped, []byte(badRule), 0o644)
	if _, err := app.ImportConfig(wrapped); err == nil {
		t.Fatal("含非法 Map Remote 规则的配置应被拒绝")
	}

	if _, err := app.ImportConfig(""); err == nil {
		t.Fatal("空路径应报错")
	}
}
