package mapremote

import (
	"strings"
	"testing"

	"github.com/kerison/flowlens/internal/config"
)

func TestRewriteHostOnly(t *testing.T) {
	rule := Rule{Target: "https://api.beta.com:8443"}
	got, err := Rewrite(rule, "http://api.prod.com/api/users?id=1")
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	want := "https://api.beta.com:8443/api/users?id=1"
	if got != want {
		t.Fatalf("改写结果错误：got %s want %s", got, want)
	}
}

func TestRewriteWithPrefix(t *testing.T) {
	rule := Rule{Target: "https://new.com/v2"}
	got, err := Rewrite(rule, "http://old.com/api/users")
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	want := "https://new.com/v2/api/users"
	if got != want {
		t.Fatalf("改写结果错误：got %s want %s", got, want)
	}
}

func TestRewritePrefixNoDoubleSlash(t *testing.T) {
	rule := Rule{Target: "http://new.com/v2/"}
	got, err := Rewrite(rule, "http://old.com/api/users")
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if got != "http://new.com/v2/api/users" {
		t.Fatalf("尾斜杠应被吸收，got %s", got)
	}
}

func TestRewriteEmptyPath(t *testing.T) {
	rule := Rule{Target: "https://mirror.com/v2"}
	got, err := Rewrite(rule, "http://origin.com")
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if got != "https://mirror.com/v2/" {
		t.Fatalf("空路径应补 /，got %s", got)
	}
}

func TestRewriteInvalidTarget(t *testing.T) {
	for _, target := range []string{"", "ftp://x.com", "https://"} {
		if _, err := Rewrite(Rule{Target: target}, "http://a.com/x"); err == nil {
			t.Fatalf("目标 %q 应报错", target)
		}
	}
}

func TestResolverMatch(t *testing.T) {
	res, err := New([]config.MapRemoteRule{
		{Enabled: false, Pattern: "a.com", Target: "https://x.com"},
		{Enabled: true, Pattern: "api/users", Target: "https://beta.com"},
		{Enabled: true, Pattern: "re:^https://cdn\\.", Target: "https://cdn2.com"},
	})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if res.Len() != 2 {
		t.Fatalf("未启用的规则不应计入，可用 %d 条", res.Len())
	}
	if _, ok := res.Match("https://a.com/api/users"); !ok {
		t.Fatal("子串应命中")
	}
	if _, ok := res.Match("https://cdn.example.com/x.js"); !ok {
		t.Fatal("正则应命中")
	}
	if _, ok := res.Match("https://other.com/"); ok {
		t.Fatal("不应命中")
	}
}

func TestNewAggregatesErrors(t *testing.T) {
	_, err := New([]config.MapRemoteRule{
		{Enabled: true, Pattern: "re:([", Target: "https://a.com"},
		{Enabled: true, Pattern: "ok", Target: "no-scheme"},
	})
	if err == nil {
		t.Fatal("编译错误应聚合返回")
	}
	if !strings.Contains(err.Error(), "第 1 条") || !strings.Contains(err.Error(), "第 2 条") {
		t.Fatalf("错误应包含两条规则的问题：%v", err)
	}
}

func TestValidateRules(t *testing.T) {
	// 空行（未填）允许保存
	if err := ValidateRules([]config.MapRemoteRule{{}}); err != nil {
		t.Fatalf("空规则不应报错: %v", err)
	}
	// 缺匹配式
	if err := ValidateRules([]config.MapRemoteRule{{Enabled: true, Target: "https://a.com"}}); err == nil {
		t.Fatal("缺匹配式应报错")
	}
	// 目标缺协议
	if err := ValidateRules([]config.MapRemoteRule{{Enabled: true, Pattern: "x", Target: "a.com"}}); err == nil {
		t.Fatal("目标缺协议应报错")
	}
	// 合法规则
	if err := ValidateRules([]config.MapRemoteRule{{Enabled: true, Pattern: "x", Target: "https://a.com"}}); err != nil {
		t.Fatalf("合法规则不应报错: %v", err)
	}
}
