package proxy

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kerison/flowlens/internal/config"
)

// newTestConfig 构造一份开启了代理认证的配置。
func newTestConfig(t *testing.T, user, pass string) *config.Manager {
	t.Helper()
	m, err := config.NewManagerAt(t.TempDir())
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	if err := m.Update(func(c *config.Config) error {
		c.RequireAuth = true
		c.AuthUsername = user
		c.AuthPassword = pass
		return nil
	}); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	return m
}

// basic 构造一个带代理凭据的请求头。
func withProxyAuth(req *http.Request, user, pass string) {
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
}

func TestCheckProxyAuth(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*http.Request)
		wantUser  string
		wantPass  string
		wantAllow bool
	}{
		{
			name:      "凭据正确",
			setup:     func(r *http.Request) { withProxyAuth(r, "flowlens", "s3cret") },
			wantUser:  "flowlens",
			wantPass:  "s3cret",
			wantAllow: true,
		},
		{
			name:     "没有凭据",
			setup:    func(*http.Request) {},
			wantUser: "flowlens", wantPass: "s3cret",
		},
		{
			name:     "密码错误",
			setup:    func(r *http.Request) { withProxyAuth(r, "flowlens", "wrong") },
			wantUser: "flowlens", wantPass: "s3cret",
		},
		{
			name:     "用户名错误",
			setup:    func(r *http.Request) { withProxyAuth(r, "other", "s3cret") },
			wantUser: "flowlens", wantPass: "s3cret",
		},
		{
			name:     "未配置密码时一律拒绝",
			setup:    func(r *http.Request) { withProxyAuth(r, "flowlens", "") },
			wantUser: "flowlens", wantPass: "",
		},
		{
			name: "退回 Authorization 头也能识别",
			setup: func(r *http.Request) {
				r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("flowlens:s3cret")))
			},
			wantUser: "flowlens", wantPass: "s3cret", wantAllow: true,
		},
		{
			name:     "非 Basic 方案忽略",
			setup:    func(r *http.Request) { r.Header.Set("Proxy-Authorization", "Bearer abc") },
			wantUser: "flowlens", wantPass: "s3cret",
		},
		{
			name:     "base64 损坏",
			setup:    func(r *http.Request) { r.Header.Set("Proxy-Authorization", "Basic !!!") },
			wantUser: "flowlens", wantPass: "s3cret",
		},
		{
			name: "缺少密码分隔符",
			setup: func(r *http.Request) {
				r.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("flowlens")))
			},
			wantUser: "flowlens", wantPass: "s3cret",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
			tc.setup(req)
			if got := checkProxyAuth(req, tc.wantUser, tc.wantPass); got != tc.wantAllow {
				t.Fatalf("checkProxyAuth = %v，期望 %v", got, tc.wantAllow)
			}
		})
	}
}

// TestAuthFailureWritesChallenge 锁定"失败必须带 407 challenge 且 error 非 nil"两条约定：
// 少了 error 内核会 panic，少了 header 客户端不会弹登录框。
func TestAuthFailureWritesChallenge(t *testing.T) {
	m := &Manager{cfg: newTestConfig(t, "flowlens", "s3cret")}
	fn := m.authFunc()

	// 未开启认证时直接放行（回调常驻安装，靠配置决定是否校验）
	setAuth := func(on bool) {
		t.Helper()
		if err := m.cfg.Update(func(c *config.Config) error {
			c.RequireAuth = on
			return nil
		}); err != nil {
			t.Fatalf("切换认证开关失败: %v", err)
		}
	}
	setAuth(false)

	rec := httptest.NewRecorder()
	allowed, err := fn(rec, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if err != nil || !allowed {
		t.Fatalf("未开启认证应放行：allowed=%v err=%v", allowed, err)
	}
	if h := rec.Header().Get("Proxy-Authenticate"); h != "" {
		t.Fatalf("放行时不应写 challenge，实际 %q", h)
	}

	// 运行中打开认证应立即生效，不需要重启代理
	setAuth(true)

	rec = httptest.NewRecorder()
	allowed, err = fn(rec, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if allowed {
		t.Fatal("缺少凭据时应拒绝")
	}
	if err == nil {
		t.Fatal("拒绝时必须返回非空 error，否则内核记录日志时会 panic")
	}
	if got := rec.Header().Get("Proxy-Authenticate"); !strings.Contains(got, "Basic realm=") {
		t.Fatalf("407 响应缺少 Proxy-Authenticate：%q", got)
	}
}
