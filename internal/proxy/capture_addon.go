package proxy

import (
	"net/http"
	"time"

	mitm "github.com/lqqyt2423/go-mitmproxy/proxy"

	"github.com/kerison/flowlens/internal/applog"
	"github.com/kerison/flowlens/internal/capture"
)

// captureAddon 实现 go-mitmproxy 的 Addon 接口，把内核事件转换为 观流 的会话数据。
// 内嵌 BaseAddon 以只覆写关心的钩子，内核升级新增钩子时不会编译失败。
type captureAddon struct {
	mitm.BaseAddon
	m *Manager
}

// Request 在完整请求读取完成后触发。这里完成请求侧的三件事，顺序固定：
//  1. Map Remote 改写 URL（换环境）；
//  2. 请求断点挂起（用户编辑/放行/取消）；
//  3. 交还内核发送——断点取消时请求已被合成响应短路，直接返回。
//
// 已知边界：body 超过 StreamLargeBodies 的流式请求不会进本钩子。
func (a *captureAddon) Request(f *mitm.Flow) {
	defer func() {
		if r := recover(); r != nil {
			applog.Errorf("处理请求回调时发生 panic: %v", r)
		}
	}()
	remoted := a.m.applyMapRemote(f)
	if a.m.applyRequestBreakpoint(f) {
		return // 请求已被断点取消，内核会短路回复
	}
	// 记录改写结果，供 Response 阶段入库时标注（Flow 级别传递）
	if remoted != "" {
		a.m.noteRemoted(f, remoted)
	}
}

// Response 在完整响应读取完成后触发，这是产出会话数据的主入口。
func (a *captureAddon) Response(f *mitm.Flow) {
	// 内核回调里的 panic 会直接终止连接甚至进程，必须就地兜住
	defer func() {
		if r := recover(); r != nil {
			applog.Errorf("处理响应回调时发生 panic: %v", r)
		}
	}()
	// 先应用 Map Local 再断点再入库：断点里展示并编辑的
	// 必须与最终发给客户端的内容一致，否则排查时会怀疑工具在骗人
	mapped := a.m.applyMapLocal(f)
	a.m.applyResponseBreakpoint(f)
	a.m.ingest(f, nil, "", mapped)
}

// RequestError 在请求失败时触发（DNS 失败、连接超时、上游 TLS 握手失败等）。
// 失败请求同样要记录：它们往往才是用户在排查的对象。
func (a *captureAddon) RequestError(f *mitm.Flow, err error) {
	defer func() {
		if r := recover(); r != nil {
			applog.Errorf("处理请求错误回调时发生 panic: %v", r)
		}
	}()
	a.m.ingest(f, err, "", "")
}

// WebSocketStart 在协议升级成功、连接建立时触发。
// 这里必须立刻落一条会话，否则后续帧无处归属（帧以会话 ID 为键存储）。
func (a *captureAddon) WebSocketStart(f *mitm.Flow) {
	defer func() {
		if r := recover(); r != nil {
			applog.Errorf("处理 WebSocket 建立回调时发生 panic: %v", r)
		}
	}()
	a.m.ingest(f, nil, "ws", "")
}

// WebSocketMessage 在收到一帧时触发，内核已把该帧追加到 f.WebSocket.Messages 末尾。
func (a *captureAddon) WebSocketMessage(f *mitm.Flow) {
	defer func() {
		if r := recover(); r != nil {
			applog.Errorf("处理 WebSocket 消息回调时发生 panic: %v", r)
		}
	}()
	if f.WebSocket == nil || len(f.WebSocket.Messages) == 0 {
		return
	}
	last := f.WebSocket.Messages[len(f.WebSocket.Messages)-1]
	// 内核约定 Type 为 1 表示文本帧、2 表示二进制帧
	binary := last.Type != 1
	frame := capture.NewFrame(0, last.FromClient, binary, last.Content, last.Timestamp)
	a.m.ingestFrame(f, frame)
}

// SSEStart 在识别出 text/event-stream 响应时触发。
func (a *captureAddon) SSEStart(f *mitm.Flow) {
	defer func() {
		if r := recover(); r != nil {
			applog.Errorf("处理 SSE 开始回调时发生 panic: %v", r)
		}
	}()
	a.m.ingest(f, nil, "sse", "")
}

// SSEMessage 在收到一个 SSE 事件时触发。
func (a *captureAddon) SSEMessage(f *mitm.Flow) {
	defer func() {
		if r := recover(); r != nil {
			applog.Errorf("处理 SSE 事件回调时发生 panic: %v", r)
		}
	}()
	if f.SSE == nil || len(f.SSE.Events) == 0 {
		return
	}
	last := f.SSE.Events[len(f.SSE.Events)-1]
	payload := last.Raw
	if payload == "" {
		payload = last.Data
	}
	frame := capture.NewFrame(0, false, false, []byte(payload), last.Time)
	frame.Kind = "event"
	a.m.ingestFrame(f, frame)
}

// noteRemoted 记录某会话的 Map Remote 改写目标，供 Response 阶段入库时标注。
// Flow 对象没有可挂自定义数据的字段，用会话 ID 作键的旁表传递；
// ingest 取走即删，不会累积。
func (m *Manager) noteRemoted(f *mitm.Flow, target string) {
	if f == nil {
		return
	}
	m.remotedMu.Lock()
	if m.remoted == nil {
		m.remoted = make(map[string]string)
	}
	m.remoted[f.Id.String()] = target
	m.remotedMu.Unlock()
}

// takeRemoted 取出并清除会话的 Map Remote 标注。
func (m *Manager) takeRemoted(flowID string) string {
	m.remotedMu.Lock()
	defer m.remotedMu.Unlock()
	t := m.remoted[flowID]
	delete(m.remoted, flowID)
	return t
}

// ingest 把内核 Flow 转换成 FlowDetail 并写入存储。
// kindOverride 非空时强制指定资源类型（ws/sse），因为长连接没有可用的 Content-Type。
// mapped 是命中的 Map Local 本地文件路径，非空表示响应已被替换，界面上要有明确标记。
func (m *Manager) ingest(f *mitm.Flow, flowErr error, kindOverride, mapped string) {
	if f == nil || f.Request == nil {
		return
	}
	if m.paused.load() {
		return // 暂停抓取：连接照常转发，只是不记录
	}

	cfg := m.cfg.Get()
	now := time.Now()

	brief := capture.FlowBrief{
		ID:     f.Id.String(),
		Method: f.Request.Method,
		Scheme: f.Request.URL.Scheme,
		Host:   f.Request.URL.Hostname(),
		// 端口必须单独保留：Hostname() 会丢掉端口，而抓包场景里
		// 大量服务跑在 8080/3000 这类非标准端口，丢了端口就无法还原真实 URL
		Port:       f.Request.URL.Port(),
		Path:       f.Request.URL.EscapedPath(),
		Query:      f.Request.URL.RawQuery,
		ReqSize:    int64(len(f.Request.Body)),
		StartTime:  f.StartTime,
		DurationMS: now.Sub(f.StartTime).Milliseconds(),
		Proto:      f.Request.Proto,
		Mapped:     mapped,
		Remoted:    m.takeRemoted(f.Id.String()),
	}
	if brief.Scheme == "" {
		// 显式代理下部分请求的 URL 不带 scheme，按 http 兜底（HTTPS 会走 CONNECT 分支）
		brief.Scheme = "http"
	}
	if f.ConnContext != nil {
		brief.Intercepted = f.ConnContext.Intercept
		if f.ConnContext.ClientConn != nil && f.ConnContext.ClientConn.Conn != nil {
			brief.ClientAddr = f.ConnContext.ClientConn.Conn.RemoteAddr().String()
		}
		if f.ConnContext.ServerConn != nil {
			brief.ServerAddr = f.ConnContext.ServerConn.Address
		}
	}
	if flowErr != nil {
		// 错误信息可能很长，只保留首行，避免列表被撑爆
		brief.Error = firstLine(flowErr.Error())
	}

	var reqBody, resBody *capture.BodyView
	if raw, err := f.Request.DecodedBody(); err == nil {
		reqBody = capture.BuildBodyView(raw, f.Request.Header.Get("Content-Type"), cfg.MaxBodyBytes)
	}

	if f.Response != nil {
		brief.StatusCode = f.Response.StatusCode
		brief.ContentType = f.Response.Header.Get("Content-Type")
		brief.ResSize = int64(len(f.Response.Body))
		if raw, err := f.Response.DecodedBody(); err == nil {
			resBody = capture.BuildBodyView(raw, brief.ContentType, cfg.MaxBodyBytes)
		}
	}
	if kindOverride != "" {
		brief.Kind = kindOverride
	} else {
		brief.Kind = capture.ContentKind(brief.ContentType, brief.Path)
		// 长连接的 101 响应没有可识别的 Content-Type，随后的 Response 回调会把
		// WebSocketStart 已标记好的 ws/sse 覆盖成 other，这里保留原标记
		if old, ok := m.store.Get(brief.ID); ok && (old.Kind == "ws" || old.Kind == "sse") {
			brief.Kind = old.Kind
		}
	}

	detail := &capture.FlowDetail{
		FlowBrief:  brief,
		ReqHeaders: cloneHeader(f.Request.Header),
		ResHeaders: cloneHeader(responseHeader(f)),
		ReqBody:    reqBody,
		ResBody:    resBody,
	}

	// 同一 ID 的内容更新（如 WS 建立后补上 101 响应）也要推送，否则界面上的帧计数不刷新
	b, _ := m.store.Add(detail)
	m.enqueue(b)
}

// ingestFrame 把一帧写入存储并排队推送。
func (m *Manager) ingestFrame(f *mitm.Flow, frame capture.Frame) {
	if f == nil || f.Request == nil {
		return
	}
	if m.paused.load() {
		return
	}
	id := f.Id.String()
	// 会话可能因超出上限被淘汰，此时直接丢弃，避免帧存储变成无主内存
	kept, total, dropped := m.store.AppendFrame(id, frame)
	if !kept {
		return
	}
	m.enqueueFrames(capture.FrameBatch{
		FlowID:  id,
		Frames:  []capture.Frame{frame},
		Total:   total,
		Dropped: dropped,
	})
}

// responseHeader 安全获取响应头，失败请求可能没有响应。
func responseHeader(f *mitm.Flow) http.Header {
	if f.Response == nil {
		return nil
	}
	return f.Response.Header
}

// cloneHeader 深拷贝请求头。内核在连接复用时会复用 Header map，
// 不拷贝会导致历史记录被后续请求悄悄改写。
func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	return h.Clone()
}

// firstLine 截取多行错误的首行。
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}
