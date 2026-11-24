package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 这些用例锁定"认证配置不允许出现假安全状态"这条约定：
// 勾了认证却没有密码时，要么自动关掉，要么明确报错，绝不能静默放行。

func TestValidateDisablesAuthWithoutPassword(t *testing.T) {
	c := Default()
	c.RequireAuth = true
	c.AuthPassword = "   " // 只有空白也算空

	if err := c.Validate(); err != nil {
		t.Fatalf("校验不应报错（这是自动纠偏而非错误）: %v", err)
	}
	if c.RequireAuth {
		t.Fatal("密码为空时必须关闭认证")
	}
}

func TestValidateKeepsAuthWithPassword(t *testing.T) {
	c := Default()
	c.RequireAuth = true
	c.AuthPassword = "s3cret"

	if err := c.Validate(); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if !c.RequireAuth {
		t.Fatal("填了密码的认证配置不应被改动")
	}
}

func TestValidateFillsEmptyUsername(t *testing.T) {
	c := Default()
	c.AuthUsername = "  "

	if err := c.Validate(); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if c.AuthUsername != "flowlens" {
		t.Fatalf("空用户名应回落为默认值，实际 %q", c.AuthUsername)
	}
}

// TestLoadRepairsAuthWithoutPassword 覆盖"磁盘上已有脏配置"的场景：
// 老版本或手改过的配置文件不能让应用启动失败，也不能带着空密码跑认证。
func TestLoadRepairsAuthWithoutPassword(t *testing.T) {
	dir := t.TempDir()
	raw := Default()
	raw.RequireAuth = true
	raw.AuthPassword = ""

	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	m, err := NewManagerAt(dir)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if got := m.Get(); got.RequireAuth {
		t.Fatal("加载时应关闭空密码的认证")
	}
}

func TestNormalizeHosts(t *testing.T) {
	got := normalizeHosts([]string{" Bank.COM ", ".pay.com", "", "bank.com", "PAY.com"})
	want := []string{"bank.com", "pay.com"}
	if len(got) != len(want) {
		t.Fatalf("去重结果数量不符：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项应为 %s，实际 %s", i, want[i], got[i])
		}
	}
}

// 规则清洗的取舍：空行丢掉，半填的留着但禁用——
// 丢掉半填规则等于替用户删掉他的输入，而"留但禁用"不会造成误命中。
func TestNormalizeMapLocal(t *testing.T) {
	in := []MapLocalRule{
		{}, // 空行：丢掉
		{Enabled: true, Pattern: " a ", File: " /tmp/a.json "},              // 正常：去空白后保留
		{Enabled: true, Pattern: "b"},                                       // 半填：保留但禁用
		{Enabled: true, Pattern: "c", File: "/tmp/c.json", StatusCode: 999}, // 状态码越界：归零
	}
	got := normalizeMapLocal(in)

	if len(got) != 3 {
		t.Fatalf("应保留 3 条，实际 %d：%+v", len(got), got)
	}
	if got[0].Pattern != "a" || got[0].File != "/tmp/a.json" || !got[0].Enabled {
		t.Fatalf("正常规则清洗不正确：%+v", got[0])
	}
	if got[1].Enabled {
		t.Fatal("缺文件的规则应被禁用，否则会命中所有请求却读不到内容")
	}
	if got[2].StatusCode != 0 {
		t.Fatalf("越界状态码应归零，实际 %d", got[2].StatusCode)
	}
}

// 保存路径上的严格校验由 maplocal.ValidateRules 负责（含文件存在性），
// 配置层只做静态纠偏：不能因为磁盘上文件被临时移走就让应用启动失败。
func TestValidateOnlyFixesStructure(t *testing.T) {
	c := Default()
	c.MapLocalRules = []MapLocalRule{{Enabled: true, Pattern: "x", File: "/nonexistent/y.json"}}

	if err := c.Validate(); err != nil {
		t.Fatalf("不应因规则问题报错: %v", err)
	}
	// 文件不存在属于运行期问题：保持启用，让代理在命中时记日志并放行走上游
	if !c.MapLocalRules[0].Enabled {
		t.Fatal("文件缺失不应在这里被静默禁用（那是 maplocal.ValidateRules 的职责）")
	}

	// 但结构上缺字段的规则必须禁用：空匹配式会命中所有请求
	c2 := Default()
	c2.MapLocalRules = []MapLocalRule{{Enabled: true, Pattern: "x"}}
	_ = c2.Validate()
	if c2.MapLocalRules[0].Enabled {
		t.Fatal("缺文件的规则应被禁用")
	}
}
