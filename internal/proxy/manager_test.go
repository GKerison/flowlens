package proxy_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/events"
	"github.com/kerison/flowlens/internal/proxy"
)

// 这些测试覆盖 M1 的核心链路：代理启动 → 请求转发 → 会话入库 → 事件推送。
// 不使用真实外网，全部走本地 httptest，保证 CI 可离线运行。

func TestProxyCapturesPlainHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	}))
	defer upstream.Close()

	mgr, store, addrs := startProxy(t, nil)

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse("http://" + addrs)
			},
		},
	}
	resp, err := client.Get(upstream.URL + "/hello")
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("状态码应为 201，实际 %d", resp.StatusCode)
	}
	if string(body) != `{"ok":true,"path":"/hello"}` {
		t.Fatalf("响应体被改写: %s", body)
	}

	brief := waitForFlow(t, store, 3*time.Second)
	if brief.Method != http.MethodGet || brief.StatusCode != http.StatusCreated {
		t.Fatalf("会话记录不正确: %+v", brief)
	}
	if brief.Host != "127.0.0.1" || brief.Path != "/hello" {
		t.Fatalf("会话 URL 解析不正确: host=%s path=%s", brief.Host, brief.Path)
	}

	detail, ok := store.Get(brief.ID)
	if !ok {
		t.Fatal("按 ID 取详情失败")
	}
	if detail.ResBody == nil || detail.ResBody.Kind != "json" {
		t.Fatalf("响应体应被识别为 json，实际 %+v", detail.ResBody)
	}
	if detail.ResBody.Preview != `{"ok":true,"path":"/hello"}` {
		t.Fatalf("响应体内容不正确: %s", detail.ResBody.Preview)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("停止代理失败: %v", err)
	}
}

func TestProxyDecryptsHTTPS(t *testing.T) {
	if os.Getenv("FLOWLENS_NET_TEST") != "1" {
		t.Skip("需要外网连通性，设置 FLOWLENS_NET_TEST=1 后运行")
	}
	// 说明：内核在 CONNECT 到 IP 直连的主机时无法构造上游 SNI（RFC 6066 禁止对 IP 发 SNI），
	// 因此这里必须用域名做用例；本地 httptest 只能提供 IP，无法覆盖解密链路。
	caMgr, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	mgr, store, addrs := startProxy(t, caMgr)

	pool := x509.NewCertPool()
	pool.AddCert(caMgr.GetRootCA())

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse("http://" + addrs)
			},
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	resp, err := client.Get("https://example.com/")
	if err != nil {
		t.Fatalf("经代理访问 HTTPS 失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("HTTPS 响应体为空，可能未成功解密")
	}

	brief := waitForFlow(t, store, 5*time.Second)
	if brief.Scheme != "https" || !brief.Intercepted {
		t.Fatalf("HTTPS 会话未被解密: %+v", brief)
	}
	detail, _ := store.Get(brief.ID)
	if detail.ResBody == nil || detail.ResBody.Size == 0 {
		t.Fatalf("未能读取解密后的响应体: %+v", detail.ResBody)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("停止代理失败: %v", err)
	}
}

func TestEventBatchEmitted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	caMgr, _ := ca.NewManager(t.TempDir())
	mgr, _, addrs := startProxy(t, caMgr)

	got := make(chan []capture.FlowBrief, 4)
	mgr.SetEmitter(func(event string, payload any) {
		if event != events.EventFlows {
			return
		}
		batch, ok := payload.([]capture.FlowBrief)
		if !ok {
			return
		}
		select {
		case got <- batch:
		default:
		}
	})

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + addrs) },
		},
	}
	for i := 0; i < 3; i++ {
		resp, err := client.Get(upstream.URL + "/batch")
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()
	}

	select {
	case batch := <-got:
		if len(batch) == 0 {
			t.Fatal("批量事件为空")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未在超时内收到批量推送事件")
	}
	_ = mgr.Stop()
}

// startProxy 在空闲端口上启动代理，返回管理器、存储与监听地址。
// caMgr 为 nil 时自行创建。
func startProxy(t *testing.T, caMgr *ca.Manager) (*proxy.Manager, *capture.Store, string) {
	t.Helper()
	return startProxyOpts(t, caMgr, nil)
}

// startProxyOpts 同上，但允许在启动前改配置（如放开上游证书校验）。
func startProxyOpts(t *testing.T, caMgr *ca.Manager, mutate func(*config.Config)) (*proxy.Manager, *capture.Store, string) {
	t.Helper()
	mgr, store, addr, _ := startProxyFull(t, caMgr, mutate)
	return mgr, store, addr
}

// startProxyFull 与 startProxyOpts 相同，额外返回配置管理器，
// 供"运行中改配置 → 立即生效"这类用例使用。
func startProxyFull(t *testing.T, caMgr *ca.Manager, mutate func(*config.Config)) (*proxy.Manager, *capture.Store, string, *config.Manager) {
	t.Helper()

	port := freePort(t)
	cfg, err := config.NewManagerAt(t.TempDir())
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	err = cfg.Update(func(c *config.Config) error {
		c.ListenHost = "127.0.0.1"
		c.ProxyPort = port
		if mutate != nil {
			mutate(c)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	if caMgr == nil {
		caMgr, err = ca.NewManager(t.TempDir())
		if err != nil {
			t.Fatalf("创建 CA 失败: %v", err)
		}
	}

	store := capture.NewStore(100)
	mgr := proxy.NewManager(cfg, caMgr, store, nil)
	if _, err := mgr.Start(); err != nil {
		t.Fatalf("启动代理失败: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop() })

	return mgr, store, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), cfg
}

// freePort 申请一个临时端口后立即释放，用作测试用的代理端口。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForFlow 等待至少一条会话入库，返回最新一条。
func waitForFlow(t *testing.T, store *capture.Store, timeout time.Duration) capture.FlowBrief {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		flows := store.ListSince(0)
		if len(flows) > 0 {
			return flows[len(flows)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("超时未捕获到会话")
	return capture.FlowBrief{}
}

// TestProxyAuthGate 覆盖"局域网模式必须能开认证"这条链路：
// 无凭据 → 407（且带 challenge）；凭据正确 → 正常转发；凭据错误 → 407；
// 运行中关闭认证 → 立即放行（不需要重启代理）。
func TestProxyAuthGate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("through"))
	}))
	defer upstream.Close()

	mgr, store, addr, cfg := startProxyFull(t, nil, func(c *config.Config) {
		c.RequireAuth = true
		c.AuthUsername = "u1"
		c.AuthPassword = "p1"
	})

	// 每个请求单独起 Transport，避免连接复用把上一次的凭据带过去
	get := func(proxyAddr string) (*http.Response, error) {
		u, err := url.Parse(proxyAddr)
		if err != nil {
			t.Fatalf("解析代理地址失败: %v", err)
		}
		tr := &http.Transport{Proxy: http.ProxyURL(u)}
		defer tr.CloseIdleConnections()
		client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
		return client.Get(upstream.URL + "/gated")
	}

	resp, err := get("http://" + addr)
	if err != nil {
		t.Fatalf("无凭据请求应拿到 407 而不是连接错误: %v", err)
	}
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("无凭据应返回 407，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Proxy-Authenticate") == "" {
		t.Fatal("407 缺少 Proxy-Authenticate，客户端不会弹登录框")
	}
	_ = resp.Body.Close()

	resp, err = get("http://u1:p1@" + addr)
	if err != nil {
		t.Fatalf("带凭据请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "through" {
		t.Fatalf("带凭据应正常转发，实际 %d %q", resp.StatusCode, body)
	}
	if store.Len() != 1 {
		t.Fatalf("已放行的请求应入库，当前会话数 %d", store.Len())
	}

	resp, err = get("http://u1:wrong@" + addr)
	if err != nil {
		t.Fatalf("错误凭据不应导致连接错误: %v", err)
	}
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("错误凭据应返回 407，实际 %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 运行中关掉认证：配置热生效，无需重启
	if err := cfg.Update(func(c *config.Config) error {
		c.RequireAuth = false
		return nil
	}); err != nil {
		t.Fatalf("关闭认证失败: %v", err)
	}
	resp, err = get("http://" + addr)
	if err != nil {
		t.Fatalf("关闭认证后请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("关闭认证后应直接放行，实际 %d", resp.StatusCode)
	}

	if !mgr.State().Running {
		t.Fatal("认证开关不应影响代理运行状态")
	}
}
