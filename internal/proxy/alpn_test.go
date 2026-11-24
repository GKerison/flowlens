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

// TestH2OnlyClientFallsBackToHTTP1 覆盖 iPhone Safari 的典型握手：
// CONNECT 之后的 ClientHello 只声明 h2，不声明 http/1.1。
//
// 上游内核原版在惰性握手里固定 NextProtos=["http/1.1"]，两者无交集，
// Go 的 negotiateALPN 回 no_application_protocol，连接直接被丢弃，
// 表现为"有些包抓不到"且日志出现 client requested unsupported application protocols。
// 本地补丁改为不声明 ALPN，客户端按 RFC 7540 §3.3 回落 HTTP/1.1。
func TestH2OnlyClientFallsBackToHTTP1(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "captured over http/1.1")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	caMgr := mustCA(t)
	_, store, proxyAddr := startProxyOpts(t, caMgr, func(c *config.Config) {
		c.InsecureUpstream = true // httptest 上游是自签证书
	})

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caMgr.Info().PEM)) {
		t.Fatal("CA 根证书无法加入信任池")
	}

	// 手工建立 CONNECT 隧道：http.Transport 会自动改写 NextProtos，
	// 无法精确控制"客户端只提供 h2"这一条件。
	conn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer conn.Close()

	target := upstreamURL.Host
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	connected, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("读取 CONNECT 响应失败: %v", err)
	}
	if connected.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT 期望 200，实际 %d", connected.StatusCode)
	}

	// 用域名做 SNI（IP 直连本身不带 SNI，属另一类问题），但 ALPN 只给 h2。
	const sni = "h2-only.test"
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: sni,
		RootCAs:    pool,
		NextProtos: []string{"h2"},
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("仅提供 h2 的客户端握手失败: %v", err)
	}
	if got := tlsConn.ConnectionState().NegotiatedProtocol; got != "" {
		t.Fatalf("惰性模式不应协商出 ALPN，实际 %q", got)
	}

	if _, err := fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", sni); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("HTTP/1.1 请求失败: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "captured over http/1.1") {
		t.Fatalf("响应异常: %d %q", res.StatusCode, body)
	}

	_ = waitForFlow(t, store, 3*time.Second)
}

// TestIPDirectWithoutSNI 覆盖 IP 直连：ClientHello 不带 SNI，
// 原版会用空主机名签发证书失败，导致这类请求完全抓不到。
func TestIPDirectWithoutSNI(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ip direct captured")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	caMgr := mustCA(t)
	_, store, proxyAddr := startProxyOpts(t, caMgr, func(c *config.Config) {
		c.InsecureUpstream = true
	})

	conn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := upstreamURL.Host
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	connected, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || connected.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT 失败: %v / %d", err, connected.StatusCode)
	}

	// 故意不设 ServerName：Go 不会发送 SNI，模拟 IP 直连。
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // 测试客户端
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("无 SNI 握手失败: %v", err)
	}
	if _, err := fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("HTTP 请求失败: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "ip direct captured") {
		t.Fatalf("响应异常: %d %q", res.StatusCode, body)
	}
	_ = waitForFlow(t, store, 3*time.Second)
}
