package maplocal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kerison/flowlens/internal/config"
)

func TestCompileSubstring(t *testing.T) {
	m, err := Compile("api/users")
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if !m("https://a.com/api/users?page=1") {
		t.Fatal("子串匹配应命中")
	}
	if m("https://a.com/api/orders") {
		t.Fatal("子串不匹配时不应命中")
	}
}

func TestCompileGlobIsAnchored(t *testing.T) {
	m, err := Compile("https://a.com/api/*")
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if !m("https://a.com/api/v1/users") {
		t.Fatal("通配应命中")
	}
	// 锚定是关键：不锚定的话任何 URL 只要带上这段前缀就能被 mock，
	// 用户会莫名其妙发现"别的域名的请求也被替换了"
	if m("https://b.com/redirect?to=https://a.com/api/1") {
		t.Fatal("通配式必须整串匹配，不应命中出现在参数里的地址")
	}
}

func TestCompileGlobSingleChar(t *testing.T) {
	m, err := Compile("https://a.com/v?/list")
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if !m("https://a.com/v1/list") {
		t.Fatal("? 应匹配单个字符")
	}
	if m("https://a.com/v12/list") {
		t.Fatal("? 不应匹配多个字符")
	}
}

func TestCompileRegex(t *testing.T) {
	m, err := Compile(`re:^https://a\.com/v\d+/user/\d+$`)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if !m("https://a.com/v2/user/42") {
		t.Fatal("正则应命中")
	}
	if m("https://a.com/v2/user/abc") {
		t.Fatal("正则不匹配时不应命中")
	}
}

func TestCompileInvalid(t *testing.T) {
	if _, err := Compile("   "); err == nil {
		t.Fatal("空匹配式应报错")
	}
	if _, err := Compile(`re:([a-z`); err == nil {
		t.Fatal("非法正则应报错")
	}
}

func TestMatchFirstHitWins(t *testing.T) {
	res, err := New([]config.MapLocalRule{
		{Enabled: true, Pattern: "api/users/1", File: "/tmp/special.json"},
		{Enabled: true, Pattern: "api/users", File: "/tmp/generic.json"},
	})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	rule, ok := res.Match("https://a.com/api/users/1")
	if !ok || rule.File != "/tmp/special.json" {
		t.Fatalf("应按顺序取第一条命中，实际 %+v ok=%v", rule, ok)
	}
}

func TestDisabledRuleIgnored(t *testing.T) {
	res, err := New([]config.MapLocalRule{
		{Enabled: false, Pattern: "*", File: "/tmp/x.json"},
	})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if res.Len() != 0 {
		t.Fatalf("禁用规则不应参与匹配，实际 %d 条", res.Len())
	}
	if _, ok := res.Match("https://a.com/anything"); ok {
		t.Fatal("禁用规则不应命中")
	}
}

func TestNewReportsBrokenRule(t *testing.T) {
	res, err := New([]config.MapLocalRule{
		{Enabled: true, Pattern: `re:([a-z`, File: "/tmp/x.json"},
		{Enabled: true, Pattern: "ok", File: "/tmp/y.json"},
	})
	if err == nil {
		t.Fatal("坏规则必须报错，否则用户永远不知道它没生效")
	}
	// 坏规则被跳过，好规则仍然可用——一条写错不该让全部 mock 失效
	if res.Len() != 1 {
		t.Fatalf("应保留 1 条可用规则，实际 %d", res.Len())
	}
}

func TestLoadContentType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	body, ct, err := Load(Rule{File: path})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("内容不一致: %s", body)
	}
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("扩展名应推断为 json，实际 %q", ct)
	}

	// 显式指定的类型优先于扩展名
	_, ct, err = Load(Rule{File: path, ContentType: "application/vnd.api+json"})
	if err != nil || ct != "application/vnd.api+json" {
		t.Fatalf("应使用显式 Content-Type，实际 %q err=%v", ct, err)
	}
}

func TestLoadErrors(t *testing.T) {
	if _, _, err := Load(Rule{File: filepath.Join(t.TempDir(), "nope.json")}); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if _, _, err := Load(Rule{File: t.TempDir()}); err == nil {
		t.Fatal("目录应报错")
	}
	if _, _, err := Load(Rule{}); err == nil {
		t.Fatal("空路径应报错")
	}
}

func TestValidateRules(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "a.json")
	if err := os.WriteFile(ok, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		rules []config.MapLocalRule
		want  string // 错误里应包含的片段，空表示应通过
	}{
		{
			name:  "界面新增的空行可保存",
			rules: []config.MapLocalRule{{Enabled: false}},
			want:  "",
		},
		{
			name:  "合法规则",
			rules: []config.MapLocalRule{{Enabled: true, Pattern: "api/users", File: ok}},
			want:  "",
		},
		{
			name:  "缺少匹配式",
			rules: []config.MapLocalRule{{Enabled: true, File: ok}},
			want:  "匹配式不能为空",
		},
		{
			name:  "正则写错",
			rules: []config.MapLocalRule{{Enabled: true, Pattern: "re:[", File: ok}},
			want:  "正则",
		},
		{
			name:  "启用的规则文件不存在",
			rules: []config.MapLocalRule{{Enabled: true, Pattern: "x", File: filepath.Join(dir, "gone.json")}},
			want:  "不可用",
		},
		{
			name:  "未启用的规则不要求文件存在",
			rules: []config.MapLocalRule{{Enabled: false, Pattern: "x", File: filepath.Join(dir, "gone.json")}},
			want:  "",
		},
		{
			name:  "状态码越界",
			rules: []config.MapLocalRule{{Enabled: true, Pattern: "x", File: ok, StatusCode: 999}},
			want:  "状态码",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRules(c.rules)
			if c.want == "" {
				if err != nil {
					t.Fatalf("不应报错，实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错并包含 %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}
