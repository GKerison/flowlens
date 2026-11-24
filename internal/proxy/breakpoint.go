package proxy

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	mitm "github.com/lqqyt2423/go-mitmproxy/proxy"

	"github.com/kerison/flowlens/internal/applog"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/events"
)

// NameValuePair 是有序头键值对。用数组而非 map：保留重复头（多个 Set-Cookie）与顺序。
type NameValuePair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// BreakpointPayload 是断点暂停时推送给界面的完整上下文。
// 界面据此渲染编辑表单；HoldKey 用于放行时回传。
type BreakpointPayload struct {
	HoldKey string          `json:"holdKey"`
	FlowID  string          `json:"flowId"`
	Stage   string          `json:"stage"` // request | response
	Method  string          `json:"method"`
	URL     string          `json:"url"`
	Headers []NameValuePair `json:"headers"`
	Body    string          `json:"body"`
	// BodyBinary 内容不是纯文本（图片/protobuf 等）时置位：界面隐藏编辑框，只留放行/取消。
	BodyBinary bool `json:"bodyBinary"`
	// BodyTruncated 内容超出展示上限时置位：编辑保存会被拒绝，避免静默截断数据。
	BodyTruncated bool `json:"bodyTruncated"`

	// 以下仅 response 阶段有值
	StatusCode int             `json:"statusCode,omitempty"`
	ResHeaders []NameValuePair `json:"resHeaders,omitempty"`
}

// BreakpointDecision 是用户对一条断点的处置结果。
type BreakpointDecision struct {
	Action string `json:"action"` // forward | drop
	Method string `json:"method"`
	URL    string `json:"url"`
	// Headers 为 nil 表示沿用原头；非 nil（可为空数组）表示整体替换
	Headers []NameValuePair `json:"headers"`
	// Body 为 nil 表示沿用原 body；指针指向空串表示清空
	Body *string `json:"body"`
	// StatusCode 仅 response 阶段有效；0 表示沿用原状态码
	StatusCode int `json:"statusCode"`
}

// bpHold 是一条等待用户处置的断点。
type bpHold struct {
	key string
	ch  chan BreakpointDecision
}

// bpTimeoutDefault 兜底超时：配置被破坏时也不能无限等待。
const bpTimeoutDefault = 120 * time.Second

// breakpointEnabled 返回断点是否开启以及本次请求命中的阶段。
func (m *Manager) breakpointStage(stage string) bool {
	cfg := m.cfg.Get()
	if !cfg.BreakpointEnabled {
		return false
	}
	switch stage {
	case "request":
		return cfg.BreakpointRequest
	case "response":
		return cfg.BreakpointResponse
	default:
		return false
	}
}

// bpTimeout 返回本次断点的等待时长（配置现读，改动即时生效）。
func (m *Manager) bpTimeout() time.Duration {
	sec := m.cfg.Get().BreakpointTimeout
	if sec < config.MinBreakpointTimeout || sec > config.MaxBreakpointTimeout {
		sec = config.DefaultBreakpointTimeout
	}
	return time.Duration(sec) * time.Second
}

// waitBreakpoint 注册断点、推送事件并阻塞等待用户处置或超时。
// 返回 nil 表示放行原内容（超时或通道关闭）。
func (m *Manager) waitBreakpoint(flowID, stage string, payload *BreakpointPayload) *BreakpointDecision {
	key := flowID + ":" + stage
	hold := &bpHold{key: key, ch: make(chan BreakpointDecision, 1)}

	m.bpMu.Lock()
	if m.bpHolds == nil {
		m.bpHolds = make(map[string]*bpHold)
	}
	m.bpHolds[key] = hold
	m.bpMu.Unlock()

	defer func() {
		m.bpMu.Lock()
		delete(m.bpHolds, key)
		m.bpMu.Unlock()
	}()

	payload.HoldKey = key
	m.emitBreakpoint(payload)

	timeout := m.bpTimeout()
	applog.Infof("断点暂停 %s（%s），等待处置，超时 %s", payload.URL, stage, timeout)

	select {
	case d, ok := <-hold.ch:
		if !ok {
			return nil
		}
		applog.Infof("断点 %s 处置：%s", key, d.Action)
		return &d
	case <-time.After(timeout):
		applog.Warnf("断点 %s 超时未处置，自动放行原内容", key)
		return nil
	}
}

// emitBreakpoint 推送断点事件。emit 为空（单测场景）时跳过。
func (m *Manager) emitBreakpoint(p *BreakpointPayload) {
	m.emitMu.RLock()
	emit := m.emit
	m.emitMu.RUnlock()
	if emit == nil {
		return
	}
	emit(events.EventBreakpoint, p)
}

// ResolveBreakpoint 由界面调用：把处置结果投递给正在等待的断点。
// 返回 false 表示该断点已不存在（已超时放行或代理已停止）。
func (m *Manager) ResolveBreakpoint(holdKey string, d BreakpointDecision) bool {
	m.bpMu.Lock()
	hold, ok := m.bpHolds[holdKey]
	m.bpMu.Unlock()
	if !ok {
		return false
	}
	hold.ch <- d
	return true
}

// PendingBreakpoints 返回当前等待处置的断点数（界面徽标用）。
func (m *Manager) PendingBreakpoints() int {
	m.bpMu.Lock()
	defer m.bpMu.Unlock()
	return len(m.bpHolds)
}

// applyRequestBreakpoint 在请求阶段挂起：用户可改方法/URL/头/体后放行，或直接取消。
// 返回 true 表示请求已被取消（内核会短路回复，调用方应立即返回）。
//
// 已知边界：body 超过 StreamLargeBodies 的请求走流式转发，不会进 Request 钩子，断点不生效。
func (m *Manager) applyRequestBreakpoint(f *mitm.Flow) bool {
	if f == nil || f.Request == nil || f.Request.URL == nil {
		return false
	}
	if !m.breakpointStage("request") {
		return false
	}

	payload := m.buildRequestPayload(f)
	decision := m.waitBreakpoint(f.Id.String(), "request", payload)

	if decision == nil {
		return false // 超时放行：保持原样
	}

	if decision.Action == "drop" {
		// 内核对 Request 钩子里合成的 Response 走短路回复且不再触发 Response 钩子，
		// 所以这里必须手动入库，否则这条"被取消的请求"在界面上无迹可寻。
		if f.Response == nil {
			f.Response = &mitm.Response{
				StatusCode: 503,
				Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
				Body:       []byte("观流: 请求已在断点中取消"),
			}
		}
		m.ingest(f, nil, "", "")
		return true
	}

	applyRequestEdits(f, decision)
	return false
}

// buildRequestPayload 从内核 Flow 提取请求上下文。
func (m *Manager) buildRequestPayload(f *mitm.Flow) *BreakpointPayload {
	cfg := m.cfg.Get()
	p := &BreakpointPayload{
		Stage:   "request",
		FlowID:  f.Id.String(),
		Method:  f.Request.Method,
		URL:     f.Request.URL.String(),
		Headers: headerPairs(f.Request.Header),
	}
	if raw, err := f.Request.DecodedBody(); err == nil {
		view := capture.BuildBodyView(raw, f.Request.Header.Get("Content-Type"), cfg.MaxBodyBytes)
		if view != nil {
			p.Body = view.Preview
			p.BodyBinary = view.Kind == "binary"
			p.BodyTruncated = view.Truncated
		}
	}
	return p
}

// applyRequestEdits 把用户的编辑落到内核 Flow 上。
func applyRequestEdits(f *mitm.Flow, d *BreakpointDecision) {
	if d.Method != "" && d.Method != f.Request.Method {
		f.Request.Method = strings.ToUpper(strings.TrimSpace(d.Method))
	}
	if u := strings.TrimSpace(d.URL); u != "" && u != f.Request.URL.String() {
		if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
			f.Request.URL = parsed
		} else if err != nil {
			applog.Warnf("断点改写 URL 无效，保留原 URL（%s）：%v", u, err)
		}
	}
	if d.Headers != nil {
		h := make(http.Header, len(d.Headers))
		for _, pair := range d.Headers {
			name := strings.TrimSpace(pair.Name)
			if name == "" {
				continue
			}
			h.Add(name, pair.Value)
		}
		f.Request.Header = h
	}
	if d.Body != nil {
		f.Request.Body = []byte(*d.Body)
		// body 已变：旧长度头必须删掉，外发请求由内核按新 body 重新计算
		f.Request.Header.Del("Content-Length")
	}
}

// applyResponseBreakpoint 在响应阶段挂起：用户可改状态码/头/体后放行，或丢弃响应。
// 必须在 Map Local 之后调用——用户编辑的应当是最终要发给客户端的内容。
//
// 已知边界：body 超过 StreamLargeBodies 的响应走流式转发，不会进 Response 钩子。
func (m *Manager) applyResponseBreakpoint(f *mitm.Flow) {
	if f == nil || f.Request == nil || f.Response == nil || f.Request.URL == nil {
		return
	}
	if !m.breakpointStage("response") {
		return
	}

	cfg := m.cfg.Get()
	payload := &BreakpointPayload{
		Stage:      "response",
		FlowID:     f.Id.String(),
		Method:     f.Request.Method,
		URL:        f.Request.URL.String(),
		StatusCode: f.Response.StatusCode,
		ResHeaders: headerPairs(f.Response.Header),
	}
	if raw, err := f.Response.DecodedBody(); err == nil {
		ct := ""
		if f.Response.Header != nil {
			ct = f.Response.Header.Get("Content-Type")
		}
		view := capture.BuildBodyView(raw, ct, cfg.MaxBodyBytes)
		if view != nil {
			payload.Body = view.Preview
			payload.BodyBinary = view.Kind == "binary"
			payload.BodyTruncated = view.Truncated
		}
	}
	if payload.Headers == nil {
		payload.Headers = []NameValuePair{}
	}

	decision := m.waitBreakpoint(f.Id.String(), "response", payload)
	if decision == nil {
		return // 超时放行
	}
	if decision.Action == "drop" {
		// 丢弃响应：以 503 短路。上游已收到请求，无法"撤回"，
		// 这里只是不让上游的响应到达客户端。
		f.Response.StatusCode = http.StatusServiceUnavailable
		f.Response.Header = http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}}
		f.Response.Body = []byte("观流: 响应已在断点中丢弃")
		return
	}
	applyResponseEdits(f, decision)
}

// applyResponseEdits 把用户对响应的编辑落到内核 Flow 上。
func applyResponseEdits(f *mitm.Flow, d *BreakpointDecision) {
	if d.StatusCode > 0 && d.StatusCode != f.Response.StatusCode {
		f.Response.StatusCode = d.StatusCode
	}
	if d.Headers != nil {
		h := make(http.Header, len(d.Headers))
		for _, pair := range d.Headers {
			name := strings.TrimSpace(pair.Name)
			if name == "" {
				continue
			}
			h.Add(name, pair.Value)
		}
		f.Response.Header = h
	}
	if d.Body != nil {
		f.Response.Body = []byte(*d.Body)
		if f.Response.Header == nil {
			f.Response.Header = make(http.Header)
		}
		// 编辑后的内容是明文：压缩与长度头都必须重算（与 Map Local 同理）
		f.Response.Header.Del("Content-Encoding")
		f.Response.Header.Set("Content-Length", strconv.Itoa(len(*d.Body)))
	}
}

// headerPairs 把 http.Header 转为有序键值对，键按字母序保证多次查看顺序稳定。
func headerPairs(h http.Header) []NameValuePair {
	if len(h) == 0 {
		return []NameValuePair{}
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sortStrings(names)
	out := make([]NameValuePair, 0, len(names))
	for _, k := range names {
		for _, v := range h[k] {
			out = append(out, NameValuePair{Name: k, Value: v})
		}
	}
	return out
}

// sortStrings 简单插入排序：头数量通常 <30，无需引入 sort 的接口开销。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Validate 校验处置结果的基本合法性，service 层直接复用。
func (d BreakpointDecision) Validate() error {
	switch d.Action {
	case "forward":
		if d.URL != "" {
			if u, err := url.Parse(strings.TrimSpace(d.URL)); err != nil || u.Host == "" {
				return errors.New("URL 无效，需为完整地址（含协议与主机名）")
			}
		}
		if d.StatusCode != 0 && (d.StatusCode < 100 || d.StatusCode > 599) {
			return errors.New("状态码需在 100-599 之间")
		}
		return nil
	case "drop":
		return nil
	default:
		return errors.New("action 需为 forward 或 drop")
	}
}
