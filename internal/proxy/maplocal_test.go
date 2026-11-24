package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/config"
)

// Map Local 是本项目唯一"改动真实流量"的功能，必须验证它改的确实是发给客户端的内容，
// 而不只是改了本地记录——后者会让用户对着抓包结果排查一个根本不存在的问题。
func TestMapLocalReplacesResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 上游故意带 gzip 声明但内容不压缩：验证我们清掉了 Content-Encoding，
		// 否则客户端会拿本地明文去解压而报错（真实场景里上游确实会压缩）
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte(`{"from":"upstream"}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	mockPath := filepath.Join(dir, "users.json")
	mock := `{"from":"local-file","users":[1,2,3]}`
	if err := os.WriteFile(mockPath, []byte(mock), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, store, addr := startProxyOpts(t, nil, func(c *config.Config) {
		c.MapLocalRules = []config.MapLocalRule{{
			Enabled: true,
			Pattern: "/api/users",
			File:    mockPath,
			// 状态码也一并覆盖：验证"只换 body 不换状态码"这个常见误区
			StatusCode: http.StatusAccepted,
		}}
	})
	_ = mgr

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + addr) },
		},
	}
	resp, err := client.Get(upstream.URL + "/api/users")
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != mock {
		t.Fatalf("客户端拿到的应是本地文件内容，实际 %q", body)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("状态码应被规则覆盖为 202，实际 %d", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("必须清掉 Content-Encoding，实际 %q", enc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("Content-Type 应按文件类型推断，实际 %q", ct)
	}

	brief := waitForFlow(t, store, 3*time.Second)
	if brief.Mapped != mockPath {
		t.Fatalf("会话应标记命中的本地文件，实际 %q", brief.Mapped)
	}
	if brief.ResSize != int64(len(mock)) {
		t.Fatalf("入库大小应是替换后的大小 %d，实际 %d", len(mock), brief.ResSize)
	}
}

// 规则不命中时必须是"完全没发生"，不能顺手改状态码或头。
func TestMapLocalNoMatchKeepsUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("real"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	mockPath := filepath.Join(dir, "x.json")
	if err := os.WriteFile(mockPath, []byte("mock"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, addr := startProxyOpts(t, nil, func(c *config.Config) {
		c.MapLocalRules = []config.MapLocalRule{{
			Enabled: true, Pattern: "/api/other", File: mockPath, StatusCode: 500,
		}}
	})

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + addr) },
		},
	}
	resp, err := client.Get(upstream.URL + "/api/users")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "real" || resp.StatusCode != http.StatusOK {
		t.Fatalf("未命中规则时应原样返回上游响应，实际 %d %q", resp.StatusCode, body)
	}
}
