package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/mapremote"
	"github.com/kerison/flowlens/internal/proxy"
)

// setMapRemote 写入远程映射规则（配置现读 + 版本号缓存失效，运行中生效）。
func setMapRemote(mgr *proxy.Manager, cfgMgr *config.Manager, rules []config.MapRemoteRule) error {
	if err := cfgMgr.Update(func(c *config.Config) error {
		c.MapRemoteRules = rules
		return nil
	}); err != nil {
		return err
	}
	_ = mgr
	return nil
}

// TestMapRemoteRewritesTarget 覆盖"命中规则 → 请求真实发往新目标 → 会话记录新 URL"整条链路。
func TestMapRemoteRewritesTarget(t *testing.T) {
	var hitOrigin, hitBeta bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOrigin = true
		_, _ = w.Write([]byte(`from-origin`))
	}))
	defer origin.Close()

	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitBeta = true
		if r.URL.Path != "/v2/api/users" {
			t.Errorf("改写后路径应为 /v2/api/users，实际 %s", r.URL.Path)
		}
		if r.URL.RawQuery != "id=7" {
			t.Errorf("查询串应保留，实际 %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`from-beta`))
	}))
	defer beta.Close()

	mgr, store, addr, cfgMgr := startProxyFull(t, nil, nil)

	originHost := strings.TrimPrefix(origin.URL, "http://")
	betaHost := strings.TrimPrefix(beta.URL, "http://")
	err := setMapRemote(mgr, cfgMgr, []config.MapRemoteRule{
		{Enabled: true, Pattern: "http://" + originHost + "/api/*", Target: "http://" + betaHost + "/v2"},
	})
	if err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + addr)
		}},
	}
	resp, err := client.Get("http://" + originHost + "/api/users?id=7")
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if hitOrigin {
		t.Fatal("请求不应到达原服务器")
	}
	if !hitBeta {
		t.Fatal("改写后的请求应到达新目标")
	}
	if string(body) != "from-beta" {
		t.Fatalf("响应应来自新目标，实际 %q", string(body))
	}

	brief := waitForFlow(t, store, 3*time.Second)
	if brief.Host != strings.Split(betaHost, ":")[0] {
		t.Fatalf("会话应记录改写后的主机，实际 %s", brief.Host)
	}
	if brief.Path != "/v2/api/users" {
		t.Fatalf("会话应记录改写后的路径，实际 %s", brief.Path)
	}
	if brief.Remoted == "" {
		t.Fatal("会话应带 Map Remote 标注")
	}
}

func TestMapRemoteUnmatchedGoesThrough(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`origin-ok`))
	}))
	defer origin.Close()

	mgr, _, addr, cfgMgr := startProxyFull(t, nil, nil)
	originHost := strings.TrimPrefix(origin.URL, "http://")
	if err := setMapRemote(mgr, cfgMgr, []config.MapRemoteRule{
		{Enabled: true, Pattern: "will-not-match.example", Target: "http://127.0.0.1:1"},
	}); err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + addr)
		}},
	}
	resp, err := client.Get("http://" + originHost + "/x")
	if err != nil {
		t.Fatalf("未命中的请求应原样转发: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "origin-ok" {
		t.Fatalf("未命中应保持原响应，实际 %q", string(body))
	}
}

// 编译期确认 mapremote 包被引用（防止 import 被误删后语义漂移没人发现）。
var _ = mapremote.Rewrite
