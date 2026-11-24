package replay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/capture"
)

func newSource(t *testing.T, method, target, body string) *capture.FlowDetail {
	t.Helper()
	u := strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
	host, path, _ := strings.Cut(u, "/")
	scheme := "http"
	if strings.HasPrefix(target, "https://") {
		scheme = "https"
	}
	d := &capture.FlowDetail{
		FlowBrief: capture.FlowBrief{
			ID:        "src-1",
			Method:    method,
			Scheme:    scheme,
			Host:      host,
			Path:      "/" + path,
			StartTime: time.Now(),
		},
		ReqHeaders: http.Header{},
	}
	if body != "" {
		d.ReqHeaders.Set("Content-Type", "application/json")
		d.ReqBody = capture.BuildBodyView([]byte(body), "application/json", 0)
	}
	return d
}

func TestReplayGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "abc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	src := newSource(t, "GET", srv.URL+"/ping", "")
	src.ReqHeaders.Set("X-Token", "abc")
	// 逐跳头部不应被转发
	src.ReqHeaders.Set("Connection", "keep-alive")

	r, err := New(Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Do(src)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if got.StatusCode != 200 {
		t.Fatalf("状态码应为 200，实际 %d", got.StatusCode)
	}
	if got.ResBody == nil || !strings.Contains(got.ResBody.Preview, `"ok":true`) {
		t.Fatalf("响应体不正确: %+v", got.ResBody)
	}
	if !got.Replayed {
		t.Fatal("重放结果应标记 Replayed=true")
	}
	if got.ID == src.ID || got.ID == "" {
		t.Fatal("重放必须生成新 ID")
	}
	if got.ReqHeaders.Get("Connection") != "" {
		t.Fatal("逐跳头部 Connection 不应被转发")
	}
}

func TestReplayPOSTBody(t *testing.T) {
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	src := newSource(t, "POST", srv.URL+"/submit", `{"name":"张三"}`)
	r, _ := New(Options{})
	got, err := r.Do(src)
	if err != nil {
		t.Fatal(err)
	}
	if received != `{"name":"张三"}` {
		t.Fatalf("服务端收到的请求体不正确: %q", received)
	}
	if got.StatusCode != http.StatusCreated {
		t.Fatalf("状态码应为 201，实际 %d", got.StatusCode)
	}
}

func TestReplayDoesNotFollowRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	src := newSource(t, "GET", srv.URL+"/jump", "")
	r, _ := New(Options{})
	got, err := r.Do(src)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != http.StatusFound {
		t.Fatalf("应返回 302 本身而不是跟随后的结果，实际 %d", got.StatusCode)
	}
}

func TestReplayRejectsBinaryBody(t *testing.T) {
	src := newSource(t, "POST", "http://example.com/upload", "")
	src.ReqHeaders.Set("Content-Type", "application/octet-stream")
	src.ReqBody = capture.BuildBodyView([]byte{0x00, 0x01, 0x02, 0x03}, "application/octet-stream", 0)

	r, _ := New(Options{})
	if _, err := r.Do(src); err == nil {
		t.Fatal("二进制请求体应被拒绝，避免发送被篡改的数据")
	}
}

func TestReplayTruncatedBodyRejected(t *testing.T) {
	src := newSource(t, "POST", "http://example.com/x", "")
	src.ReqBody = capture.BuildBodyView([]byte(strings.Repeat("a", 100)), "text/plain", 10)

	r, _ := New(Options{})
	_, err := r.Do(src)
	if err == nil || !strings.Contains(err.Error(), "截断") {
		t.Fatalf("被截断的请求体应拒绝重放，实际错误: %v", err)
	}
}

func TestReplayUpstreamProxy(t *testing.T) {
	var proxied bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true // 作为代理收到的是绝对 URI 请求
		w.WriteHeader(http.StatusOK)
	}))
	defer proxySrv.Close()

	src := newSource(t, "GET", "http://unreachable.invalid/path", "")
	r, err := New(Options{UpstreamProxy: proxySrv.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Do(src)
	if err != nil {
		t.Fatalf("经上游代理重放失败: %v", err)
	}
	if !proxied {
		t.Fatal("请求未经上游代理")
	}
	if got.StatusCode != 200 {
		t.Fatalf("状态码应为 200，实际 %d", got.StatusCode)
	}
}

// 编辑重放：方法/地址/请求头/请求体全部按用户输入生效。
func TestReplayOverrideAll(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotHeader string
		gotBody   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotHeader, gotBody = r.Method, r.URL.Path, r.Header.Get("X-Test"), string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := newSource(t, "GET", srv.URL+"/original", "")
	src.ReqHeaders.Set("X-Test", "old")

	r, _ := New(Options{})
	body := `{"edited":true}`
	headers := http.Header{}
	headers.Set("X-Test", "new")
	headers.Set("Content-Type", "application/json")

	got, err := r.DoWith(src, &Override{
		Method:  "post", // 小写也要被规范化
		URL:     srv.URL + "/edited",
		Headers: headers,
		Body:    &body,
	})
	if err != nil {
		t.Fatalf("编辑重放失败: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/edited" {
		t.Fatalf("方法/地址未生效: %s %s", gotMethod, gotPath)
	}
	if gotHeader != "new" {
		t.Fatalf("请求头覆盖未生效: %q", gotHeader)
	}
	if gotBody != body {
		t.Fatalf("请求体覆盖未生效: %q", gotBody)
	}
	if got.Method != "POST" || got.ReqSize != int64(len(body)) {
		t.Fatalf("新会话应记录编辑后的方法与大小，实际 %s %d", got.Method, got.ReqSize)
	}
}

// Body 指向空串表示"确实要清空请求体"，不能被当成"沿用原请求体"。
func TestReplayOverrideClearsBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	src := newSource(t, "POST", srv.URL+"/submit", `{"keep":false}`)
	empty := ""
	r, _ := New(Options{})
	got, err := r.DoWith(src, &Override{Body: &empty})
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if gotBody != "" {
		t.Fatalf("请求体应被清空，实际收到 %q", gotBody)
	}
	if got.ReqSize != 0 {
		t.Fatalf("请求大小应为 0，实际 %d", got.ReqSize)
	}
}

// 源请求体是二进制时，只要用户提供了新请求体，就应该允许重放。
func TestReplayOverrideBypassesBinaryGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := newSource(t, "POST", srv.URL+"/upload", "")
	src.ReqBody = &capture.BodyView{Kind: "binary", MIME: "application/octet-stream", Size: 1024}

	r, _ := New(Options{})
	if _, err := r.Do(src); err == nil {
		t.Fatal("原样重放二进制请求体应被拒绝")
	}
	body := "replacement"
	if _, err := r.DoWith(src, &Override{Body: &body}); err != nil {
		t.Fatalf("提供新请求体后应允许重放，实际 %v", err)
	}
}

func TestReplayOverrideRejectsUnsafeTarget(t *testing.T) {
	src := newSource(t, "GET", "http://example.invalid/x", "")
	r, _ := New(Options{})

	for _, target := range []string{"file:///etc/passwd", "gopher://x/", "/relative/path", "http://"} {
		if _, err := r.DoWith(src, &Override{URL: target}); err == nil {
			t.Fatalf("危险/非法地址 %q 应被拒绝", target)
		}
	}
}

// 改了 host 之后不能还显示原会话的服务端 IP，否则会让人误判请求打到了哪台机器。
func TestReplayOverrideDropsStaleServerAddr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	src := newSource(t, "GET", srv.URL+"/a", "")
	src.ServerAddr = "10.0.0.1:443"
	// 真实会话里 Port 由 request URL 带入；helper 不解析端口，这里补上
	src.Port = srvPort(t, srv.URL)

	r, _ := New(Options{})
	got, err := r.DoWith(src, &Override{URL: srv.URL + "/b"})
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	// host 相同（都是 127.0.0.1，端口也一致）时应保留
	if got.ServerAddr != "10.0.0.1:443" {
		t.Fatalf("同 host 应沿用服务端地址，实际 %q", got.ServerAddr)
	}

	other := newSource(t, "GET", "http://example.invalid/a", "")
	other.ServerAddr = "10.0.0.1:443"
	got2, err := r.DoWith(other, &Override{URL: srv.URL + "/c"})
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if got2.ServerAddr != "" {
		t.Fatalf("换 host 后不应沿用旧地址，实际 %q", got2.ServerAddr)
	}
}

// srvPort 从 httptest 地址里取端口，用于构造与真实会话一致的 Port 字段。
func srvPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析测试服务地址失败: %v", err)
	}
	return u.Port()
}
