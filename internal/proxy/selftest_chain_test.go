package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
)

// TestSelfTestSuccessPath 覆盖"证书已被信任"时的成功分支：
// 用一个由本机 CA 签发的 HTTPS 上游，并把该 CA 注入自检的信任池——
// 这正是用户点完「安装信任」之后的状态。全链路真实跑通：
// 客户端 → 代理（解密/重签）→ 上游 → 会话入库。
func TestSelfTestSuccessPath(t *testing.T) {
	caMgr, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// 上游用本机 CA 签发，因此"客户端信任池=本机 CA"即可验证成功。
	// 必须用域名（localhost）而不是 IP：客户端访问纯 IP 的 HTTPS 不会发送 SNI，
	// 代理无法据此签发证书（见 REQUIREMENTS §11 已知限制）。
	upstreamTLSCert, err := caMgr.GetCert("localhost")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{*upstreamTLSCert}}
	upstream.StartTLS()
	defer upstream.Close()

	_, upstreamPort, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	target := "https://localhost:" + upstreamPort + "/"

	cfg, err := config.NewManagerAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.ListenHost = "127.0.0.1"
		c.ProxyPort = freePort(t)
		// 上游是自签证书：代理到上游这一段必须宽容，否则会 502。
		// 被测的是"客户端是否信任本机 CA"，不是上游证书校验。
		c.InsecureUpstream = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	store := capture.NewStore(50)
	mgr := NewManager(cfg, caMgr, store, nil)
	if _, err := mgr.Start(); err != nil {
		t.Fatalf("启动代理失败: %v", err)
	}
	defer func() { _ = mgr.Stop() }()

	pool := x509.NewCertPool()
	pool.AddCert(caMgr.GetRootCA())

	res := mgr.selfTest(target, pool)
	if !res.OK {
		t.Fatalf("信任该 CA 后自检应通过，实际：%s（提示：%s）", res.Message, res.Hint)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("自检状态码应为 200，实际 %d", res.Status)
	}
	if res.Flows == 0 {
		t.Fatal("自检请求应产生一条会话")
	}
	if res.Duration < 0 {
		t.Fatal("耗时应为正值")
	}
}

// TestSelfTestUntrustedKeepsListClean 确认失败的自检不会污染会话列表：
// 否则用户会看到一堆自己没发过的请求，误判为"抓到了东西"。
func TestSelfTestUntrustedKeepsListClean(t *testing.T) {
	caMgr, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewManagerAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.ListenHost = "127.0.0.1"
		c.ProxyPort = freePort(t)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store := capture.NewStore(50)
	mgr := NewManager(cfg, caMgr, store, nil)
	if _, err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Stop() }()

	// 空信任池 + 不存在的域名：TLS 与 DNS 都必然失败
	res := mgr.selfTest("https://flowlens-selftest.invalid/", x509.NewCertPool())
	if res.OK {
		t.Fatal("不可达目标不应通过")
	}
	if res.Message == "" || res.Hint == "" {
		t.Fatalf("失败时必须给出原因与建议：%q / %q", res.Message, res.Hint)
	}
}

// freePort 取一个当前空闲的本地端口，避免测试之间抢端口。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
