package proxy_test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/config"
)

// httpsViaProxy 手工建立 CONNECT 隧道并完成 TLS 握手。
// rootCAs 决定客户端信任谁：传上游证书池 = 不信任我们的 MITM CA。
func httpsViaProxy(t *testing.T, proxyAddr, target string, rootCAs *x509.CertPool) (int, string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return 0, "", err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, "", err
	}
	if res.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("CONNECT 状态 %d", res.StatusCode)
	}

	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return 0, "", err
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: rootCAs})
	if err := tlsConn.Handshake(); err != nil {
		return 0, "", err
	}
	if _, err := fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host); err != nil {
		return 0, "", err
	}
	got, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		return 0, "", err
	}
	defer got.Body.Close()
	body, _ := io.ReadAll(got.Body)
	return got.StatusCode, string(body), nil
}

// TestPauseBecomesPlainTunnel 是暂停语义的核心回归：
// 暂停后代理必须退化为纯隧道，客户端与真实服务器直接握手，
// 因此即使完全不信任我们的 MITM CA 也能正常访问；
// 恢复后重新走 MITM，不信任 CA 的客户端应当被拒绝。
func TestPauseBecomesPlainTunnel(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "upstream reached directly")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	target := upstreamURL.Host

	// 客户端只信任上游自签证书，绝不把我们的 CA 放进去。
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())

	caMgr := mustCA(t)
	mgr, store, proxyAddr := startProxyOpts(t, caMgr, func(c *config.Config) {
		c.InsecureUpstream = true
	})

	// 抓包状态下：客户端不信任 MITM CA → 握手失败。
	if _, _, err := httpsViaProxy(t, proxyAddr, target, upstreamPool); err == nil {
		t.Fatal("抓包状态下不信任 MITM CA 竟然成功了，说明没有真正解密")
	}

	// 暂停后：纯隧道透传，客户端直接与上游握手 → 成功。
	if state := mgr.SetPaused(true); !state.Paused {
		t.Fatal("暂停状态未生效")
	}
	code, body, err := httpsViaProxy(t, proxyAddr, target, upstreamPool)
	if err != nil {
		t.Fatalf("暂停后应当能正常上网，实际失败: %v", err)
	}
	if code != http.StatusOK || !strings.Contains(body, "upstream reached directly") {
		t.Fatalf("透传响应异常: %d %q", code, body)
	}
	if n := len(store.ListSince(0)); n != 0 {
		t.Fatalf("暂停期间不应入库会话，实际 %d 条", n)
	}

	// 恢复后：重新走 MITM，不信任 CA 的客户端再次失败。
	mgr.SetPaused(false)
	if _, _, err := httpsViaProxy(t, proxyAddr, target, upstreamPool); err == nil {
		t.Fatal("恢复抓包后不信任 MITM CA 竟然成功了")
	}
	if n := len(store.ListSince(0)); n != 0 {
		t.Fatalf("失败握手不应产生会话，实际 %d 条", n)
	}
}
