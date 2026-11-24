package proxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
)

// mustCA 创建一个临时 CA。
func mustCA(t *testing.T) *ca.Manager {
	t.Helper()
	m, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	return m
}

// 本文件覆盖 M2 的协议细节：非 UTF-8 响应、SSE、WebSocket。
// 全部走本地 httptest，离线可跑。

func proxyClient(addrs string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse("http://" + addrs)
			},
		},
	}
}

func TestProxyDecodesGBKResponse(t *testing.T) {
	const text = "中文响应体，应该被正确解码"
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 故意声明 gbk，模拟大量国内老站点
		w.Header().Set("Content-Type", "text/plain; charset=gbk")
		_, _ = w.Write(gbk)
	}))
	defer upstream.Close()

	_, store, addrs := startProxy(t, nil)

	resp, err := proxyClient(addrs).Get(upstream.URL + "/gbk")
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	// 客户端收到的应该是原始字节（代理不改写 body）
	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != string(gbk) {
		t.Fatal("代理不应改写原始响应字节")
	}

	brief := waitForFlow(t, store, 3*time.Second)
	detail, ok := store.Get(brief.ID)
	if !ok {
		t.Fatal("取详情失败")
	}
	if detail.ResBody == nil {
		t.Fatal("响应体为空")
	}
	if detail.ResBody.Preview != text {
		t.Fatalf("GBK 未正确解码，实际 %q（编码标记 %q）", detail.ResBody.Preview, detail.ResBody.Encoding)
	}
	if detail.ResBody.Encoding != "gb18030" {
		t.Fatalf("应记录实际字符集，实际 %q", detail.ResBody.Encoding)
	}
}

func TestProxyCapturesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("不支持 flush")
			return
		}
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "event: tick\ndata: {\"n\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	_, store, addrs := startProxy(t, nil)

	resp, err := proxyClient(addrs).Get(upstream.URL + "/stream")
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// SSE 帧是逐个回调的，等到收满 3 个
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, b := range store.ListSince(0) {
			if strings.HasSuffix(b.Path, "/stream") {
				id = b.ID
			}
		}
		if id != "" {
			if frames, _ := store.Frames(id); len(frames) >= 3 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("未捕获到 SSE 会话")
	}

	frames, _ := store.Frames(id)
	if len(frames) < 3 {
		t.Fatalf("SSE 帧数不足，实际 %d", len(frames))
	}
	if !strings.Contains(frames[0].Text, `"n":1`) {
		t.Fatalf("首帧内容不正确: %q", frames[0].Text)
	}
	if frames[0].FromClient {
		t.Fatal("SSE 事件方向应为服务端 → 客户端")
	}
	if d, _ := store.Get(id); d.Kind != "sse" {
		t.Fatalf("会话类型应为 sse，实际 %q", d.Kind)
	}
}

func TestProxyCapturesWebSocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, append([]byte("echo:"), data...)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	_, store, addrs := startProxy(t, nil)

	wsURL := "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws"
	proxyURL, _ := url.Parse("http://" + addrs)
	dialer := websocket.Dialer{
		Proxy:            http.ProxyURL(proxyURL),
		HandshakeTimeout: 5 * time.Second,
	}
	conn, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("经代理建立 WebSocket 失败（HTTP %d）: %v", status, err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte("你好")); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(msg) != "echo:你好" {
		t.Fatalf("回显内容不正确: %q", msg)
	}
	_ = conn.Close()

	// 等两个方向各一帧都落到存储里
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, b := range store.ListSince(0) {
			// 明文 ws:// 走 CONNECT 隧道，内核复用 CONNECT 会话，Path 为空，
			// 因此按资源类型识别而不是按路径
			if b.Kind == "ws" {
				id = b.ID
			}
		}
		if id != "" {
			if frames, _ := store.Frames(id); len(frames) >= 2 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("未捕获到 WebSocket 会话")
	}

	frames, _ := store.Frames(id)
	if len(frames) < 2 {
		t.Fatalf("帧数不足，实际 %d 帧", len(frames))
	}

	var up, down *capture.Frame
	for i := range frames {
		if frames[i].FromClient && up == nil {
			up = &frames[i]
		}
		if !frames[i].FromClient && down == nil {
			down = &frames[i]
		}
	}
	if up == nil || down == nil {
		t.Fatalf("应同时捕获上行与下行帧，实际 %d 帧", len(frames))
	}
	if up.Text != "你好" {
		t.Fatalf("上行帧内容不正确: %q", up.Text)
	}
	if down.Text != "echo:你好" {
		t.Fatalf("下行帧内容不正确: %q", down.Text)
	}

	d, _ := store.Get(id)
	if d.Kind != "ws" {
		t.Fatalf("会话类型应为 ws，实际 %q", d.Kind)
	}
	if d.Frames < 2 {
		t.Fatalf("会话上的帧计数应同步更新，实际 %d", d.Frames)
	}
}
