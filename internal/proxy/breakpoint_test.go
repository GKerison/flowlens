package proxy_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/events"
	"github.com/kerison/flowlens/internal/proxy"
)

// 断点测试的请求超时要给足：断点挂起期间客户端会一直等。
const bpTestClientTimeout = 30 * time.Second

// bpProxy 起一个开了指定断点阶段的代理，并收集断点事件（模拟界面接收）。
func bpProxy(t *testing.T, mutate func(*config.Config)) (mgr *proxy.Manager, store *capture.Store, addr string, bpCh chan proxy.BreakpointPayload, client *http.Client) {
	t.Helper()
	mgr, st, a, _ := startProxyFull(t, nil, mutate)
	ch := make(chan proxy.BreakpointPayload, 16)
	mgr.SetEmitter(func(event string, payload any) {
		if event == events.EventBreakpoint {
			ch <- *payload.(*proxy.BreakpointPayload)
		}
	})
	c := &http.Client{
		Timeout: bpTestClientTimeout,
		Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + a)
		}},
	}
	return mgr, st, a, ch, c
}

func waitBreakpoint(t *testing.T, ch <-chan proxy.BreakpointPayload, timeout time.Duration) proxy.BreakpointPayload {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(timeout):
		t.Fatal("超时未收到断点事件")
		return proxy.BreakpointPayload{}
	}
}

func strPtr(s string) *string { return &s }

func TestBreakpointRequestEditAndForward(t *testing.T) {
	var gotHeader, gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotHeader, gotPath, gotBody = r.Header.Get("X-Test"), r.URL.Path, string(body)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	mgr, store, _, bpCh, client := bpProxy(t, func(c *config.Config) {
		c.BreakpointEnabled = true
		c.BreakpointRequest = true
	})

	done := make(chan error, 1)
	go func() {
		resp, err := client.Post(upstream.URL+"/v1/echo", "application/json", strings.NewReader(`{"orig":1}`))
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		done <- nil
	}()

	bp := waitBreakpoint(t, bpCh, 5*time.Second)
	if bp.Stage != "request" || bp.Method != "POST" || !strings.Contains(bp.URL, "/v1/echo") {
		t.Fatalf("断点载荷不正确: %+v", bp)
	}
	if bp.Body != `{"orig":1}` {
		t.Fatalf("断点 body 应为原始内容，实际 %q", bp.Body)
	}

	// 编辑放行：改头、体、路径
	newURL := strings.Replace(bp.URL, "/v1/echo", "/v2/echo", 1)
	decision := proxy.BreakpointDecision{
		Action:  "forward",
		URL:     newURL,
		Headers: []proxy.NameValuePair{{Name: "X-Test", Value: "edited"}},
		Body:    strPtr(`{"edited":2}`),
	}
	if !mgr.ResolveBreakpoint(bp.HoldKey, decision) {
		t.Fatal("投递放行决策失败")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("放行后客户端请求失败: %v", err)
		}
	case <-time.After(bpTestClientTimeout):
		t.Fatal("放行后客户端仍未收到响应")
	}

	if gotHeader != "edited" || gotBody != `{"edited":2}` {
		t.Fatalf("上游应收到编辑后的内容: header=%q body=%q", gotHeader, gotBody)
	}
	if gotPath != "/v2/echo" {
		t.Fatalf("上游应收到改写后的路径 /v2/echo，实际 %s", gotPath)
	}

	brief := waitForFlow(t, store, 3*time.Second)
	if brief.Path != "/v2/echo" {
		t.Fatalf("会话应记录改写后的路径，实际 %s", brief.Path)
	}
}

func TestBreakpointDrop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("取消的请求不应到达上游")
	}))
	defer upstream.Close()

	mgr, store, _, bpCh, client := bpProxy(t, func(c *config.Config) {
		c.BreakpointEnabled = true
		c.BreakpointRequest = true
	})

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Get(upstream.URL + "/drop-me")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		done <- result{code: resp.StatusCode}
	}()

	bp := waitBreakpoint(t, bpCh, 5*time.Second)
	if !mgr.ResolveBreakpoint(bp.HoldKey, proxy.BreakpointDecision{Action: "drop"}) {
		t.Fatal("投递取消决策失败")
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("取消应返回 503 响应而非错误: %v", r.err)
		}
		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("取消应返回 503，实际 %d", r.code)
		}
	case <-time.After(bpTestClientTimeout):
		t.Fatal("取消后客户端未收到响应")
	}

	// 被取消的请求也必须入库，否则用户无从知道它发生过
	brief := waitForFlow(t, store, 3*time.Second)
	if brief.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("取消的请求应记为 503，实际 %d", brief.StatusCode)
	}
}

func TestBreakpointTimeoutForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`original`))
	}))
	defer upstream.Close()

	mgr, _, _, bpCh, client := bpProxy(t, func(c *config.Config) {
		c.BreakpointEnabled = true
		c.BreakpointRequest = true
		c.BreakpointTimeout = config.MinBreakpointTimeout // 5s，测试可接受
	})

	start := time.Now()
	resp, err := client.Get(upstream.URL + "/timeout")
	if err != nil {
		t.Fatalf("超时放行后请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	_ = bpCh

	if string(body) != "original" {
		t.Fatalf("超时应放行原始内容，实际 %q", string(body))
	}
	if elapsed < config.MinBreakpointTimeout*time.Second {
		t.Fatalf("应在超时后才放行，实际耗时 %s", elapsed)
	}
	// 断点必须已清空，不能留下悬挂的 hold
	if n := mgr.PendingBreakpoints(); n != 0 {
		t.Fatalf("超时后不应有残留断点，实际 %d", n)
	}
}

func TestBreakpointResponseEdit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"orig"}`))
	}))
	defer upstream.Close()

	mgr, _, _, bpCh, client := bpProxy(t, func(c *config.Config) {
		c.BreakpointEnabled = true
		c.BreakpointResponse = true
	})

	done := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(upstream.URL + "/res")
		if err != nil {
			done <- nil
			return
		}
		done <- resp
	}()

	bp := waitBreakpoint(t, bpCh, 5*time.Second)
	if bp.Stage != "response" || bp.StatusCode != http.StatusOK {
		t.Fatalf("响应断点载荷不正确: %+v", bp)
	}
	if !strings.Contains(bp.Body, `"orig"`) {
		t.Fatalf("响应断点 body 应为上游内容，实际 %q", bp.Body)
	}

	edited := `{"status":"edited"}`
	if !mgr.ResolveBreakpoint(bp.HoldKey, proxy.BreakpointDecision{
		Action:     "forward",
		Body:       &edited,
		StatusCode: 201,
	}) {
		t.Fatal("投递响应编辑决策失败")
	}

	var resp1 *http.Response
	select {
	case resp1 = <-done:
	case <-time.After(bpTestClientTimeout):
		t.Fatal("放行后客户端未收到响应")
	}
	if resp1 == nil {
		t.Fatal("放行后响应为空")
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if string(body1) != edited || resp1.StatusCode != 201 {
		t.Fatalf("客户端应收到编辑后的响应（201），实际 %d %s", resp1.StatusCode, string(body1))
	}

	// 第二次请求：响应断点全局开启，仍会挂起；放行原样。
	// 注意 Get 必须放 goroutine：断点挂起期间客户端拿不到响应头，同步调用会先卡住自己。
	type get2 struct {
		resp *http.Response
		err  error
	}
	done2 := make(chan get2, 1)
	go func() {
		resp, err := client.Get(upstream.URL + "/res2")
		done2 <- get2{resp, err}
	}()
	bp2 := waitBreakpoint(t, bpCh, 5*time.Second)
	mgr.ResolveBreakpoint(bp2.HoldKey, proxy.BreakpointDecision{Action: "forward"})

	var r2 get2
	select {
	case r2 = <-done2:
	case <-time.After(bpTestClientTimeout):
		t.Fatal("第二次请求未返回")
	}
	if r2.err != nil {
		t.Fatalf("第二次请求失败: %v", r2.err)
	}
	body2, _ := io.ReadAll(r2.resp.Body)
	r2.resp.Body.Close()
	if !bytes.Contains(body2, []byte(`"orig"`)) {
		t.Fatalf("未编辑的响应应保持原样: %s", string(body2))
	}
}
