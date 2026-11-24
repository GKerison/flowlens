// Package proxy 封装 go-mitmproxy 内核：生命周期管理、抓取回调、批量事件推送。
//
// 这是整个项目**唯一** import 第三方代理库的地方。其它包只接触本文件导出的
// State 与 capture 包的 DTO，这样未来若内核需要替换或 fork，改动面收敛在此包。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lqqyt2423/go-mitmproxy/cert"
	mitm "github.com/lqqyt2423/go-mitmproxy/proxy"
	"github.com/sirupsen/logrus"

	"github.com/kerison/flowlens/internal/applog"
	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/events"
	"github.com/kerison/flowlens/internal/maplocal"
	"github.com/kerison/flowlens/internal/mapremote"
)

// flushInterval 是会话批量推送间隔。
// 取 100ms：人眼感知不到延迟，同时把高频请求合并成一次消息，避免打爆 WebView 消息通道。
const flushInterval = 100 * time.Millisecond

// startupProbe 是启动后等待内核报错的窗口，用于把"端口占用"这类错误同步返回给界面。
const startupProbe = 150 * time.Millisecond

// Emitter 是事件发送函数，由 main 注入（通常包一层 application.App.Event.Emit）。
// 之所以用函数而不是直接依赖 Wails，是为了让本包可以脱离 UI 框架做单元测试。
type Emitter func(event string, payload any)

// State 是代理运行状态快照，用于界面顶栏展示。
type State struct {
	Running   bool      `json:"running"`
	Addr      string    `json:"addr"`
	Paused    bool      `json:"paused"`
	Flows     int       `json:"flows"`
	StartedAt time.Time `json:"startedAt"`
	LastError string    `json:"lastError,omitempty"`
}

// Manager 负责代理的启动、停止、暂停与抓取数据汇聚。
type Manager struct {
	cfg   *config.Manager
	caMgr *ca.Manager
	store *capture.Store

	emitMu sync.RWMutex
	emit   Emitter

	mu        sync.Mutex
	p         *mitm.Proxy
	running   bool
	startedAt time.Time
	lastErr   string

	paused atomicBool

	queueMu sync.Mutex
	queue   []capture.FlowBrief

	// frameQueue 按会话聚合待推送的帧，避免高频 WebSocket 时逐帧发事件
	frameMu    sync.Mutex
	frameOrder []string
	frames     map[string]*capture.FrameBatch

	// Map Local 规则缓存，按配置版本号失效（见 maplocal.go）
	mlMu   sync.Mutex
	mlRev  uint64
	mlInit bool
	mlRes  *maplocal.Resolver

	// Map Remote 规则缓存，失效策略同上（见 mapremote.go）
	mrMu   sync.Mutex
	mrRev  uint64
	mrInit bool
	mrRes  *mapremote.Resolver

	// 断点：等待用户处置的挂起请求/响应（见 breakpoint.go）
	bpMu    sync.Mutex
	bpHolds map[string]*bpHold

	// remoted 是"会话 ID → Map Remote 目标"的旁表（见 capture_addon.go noteRemoted）
	remotedMu sync.Mutex
	remoted   map[string]string

	flushStop chan struct{}
	flushDone chan struct{}
}

// NewManager 创建代理管理器。emit 允许为空（测试场景），后续可用 SetEmitter 补上。
func NewManager(cfg *config.Manager, caMgr *ca.Manager, store *capture.Store, emit Emitter) *Manager {
	return &Manager{
		cfg:   cfg,
		caMgr: caMgr,
		store: store,
		emit:  emit,
	}
}

// SetEmitter 在应用实例就绪后注入事件发送函数。
func (m *Manager) SetEmitter(e Emitter) {
	m.emitMu.Lock()
	defer m.emitMu.Unlock()
	m.emit = e
}

// State 返回当前状态快照。
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{
		Running:   m.running,
		Addr:      m.currentAddrLocked(),
		Paused:    m.paused.load(),
		Flows:     m.store.Len(),
		StartedAt: m.startedAt,
		LastError: m.lastErr,
	}
}

// Addr 返回当前（或将要使用的）监听地址，如 127.0.0.1:8888。
func (m *Manager) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentAddrLocked()
}

func (m *Manager) currentAddrLocked() string {
	c := m.cfg.Get()
	return net.JoinHostPort(c.ListenHost, strconv.Itoa(c.ProxyPort))
}

// Start 启动代理。重复调用在已运行时直接返回当前状态（幂等）。
func (m *Manager) Start() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return m.stateLocked(), nil
	}
	m.lastErr = ""

	cfg := m.cfg.Get()
	addr := net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.ProxyPort))

	// 先探测端口：内核的 bind 错误是英文且信息量大，这里提前换成用户能理解的话
	if err := checkPortAvailable(addr); err != nil {
		m.lastErr = err.Error()
		return m.stateLocked(), err
	}

	p, err := mitm.NewProxy(&mitm.Options{
		Addr:              addr,
		StreamLargeBodies: cfg.StreamThreshold,
		SslInsecure:       cfg.InsecureUpstream,
		CaRootPath:        m.caMgr.Dir(),
		// 注入自研 CA：证书主体是 观流，而非内核默认的 mitmproxy
		NewCaFunc: func() (cert.CA, error) { return m.caMgr, nil },
		Upstream:  cfg.UpstreamProxy,
		Debug:     proxyDebugLevel(),
	})
	if err != nil {
		m.lastErr = fmt.Sprintf("初始化代理失败: %v", err)
		return m.stateLocked(), errors.New(m.lastErr)
	}

	p.SetShouldInterceptRule(m.shouldIntercept(cfg))
	// 认证回调常驻：内部按当前配置决定是否校验，开关认证无需重启代理
	p.SetAuthProxy(m.authFunc())
	// 关闭"先与上游握手拿证书"的行为，改用惰性连接。
	// 内核默认会在 CONNECT 后立刻与上游做 TLS 握手，导致两个问题：
	//   1. 明文 ws://（CONNECT 隧道内的非 TLS 流量）会被当成 TLS 而直接失败；
	//   2. 目标是 IP 时无法构造 SNI，TLS 握手必然失败——这也是同类工具"抓不到 IP 直连"的根因。
	// 惰性连接改为先与客户端完成握手、真正发请求时再连上游，上述两种情况都能覆盖。
	p.AddAddon(mitm.NewUpstreamCertAddon(false))
	p.AddAddon(&captureAddon{m: m})

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errCh <- fmt.Errorf("代理内核异常退出: %v", r)
			}
		}()
		errCh <- p.Start()
	}()

	// 等待启动窗口：若立即返回错误（端口被占、权限不足），直接同步报给界面
	select {
	case err = <-errCh:
		if err == nil {
			err = errors.New("代理意外退出")
		}
		m.lastErr = readableStartError(err)
		return m.stateLocked(), errors.New(m.lastErr)
	case <-time.After(startupProbe):
	}

	m.p = p
	m.running = true
	m.startedAt = time.Now()
	m.startFlushLoop()

	go m.watch(p, errCh)

	state := m.stateLocked()
	applog.Infof("代理已启动，监听 %s（解密 HTTPS：%v）", addr, cfg.InterceptTLS)
	go m.emitState(state)
	return state, nil
}

// Stop 停止代理并释放连接。未运行时静默返回。
func (m *Manager) Stop() error {
	m.mu.Lock()
	p := m.p
	m.p = nil
	m.running = false
	m.lastErr = ""
	addr := m.currentAddrLocked()
	m.mu.Unlock()

	m.stopFlushLoop()

	if p != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			// 优雅关闭失败则强制关闭监听，保证端口被释放
			applog.Warnf("代理优雅关闭失败，改为强制关闭: %v", err)
			_ = p.Close()
		}
	}

	applog.Infof("代理已停止（%s）", addr)
	state := m.State()
	go m.emitState(state)
	return nil
}

// SetPaused 暂停/恢复抓取。
//
// 暂停期间代理退化为**纯隧道中转**：不解密 HTTPS、不抓包、不执行 Map Local /
// Map Remote / 断点等改包逻辑，客户端与真实服务器直接握手。因此即使客户端
// 没信任我们的 CA，也能照常上网——表现与"没设代理"一致，只是多了一跳中转。
// 恢复后新连接自动回到正常的 MITM 解密抓包路径。
func (m *Manager) SetPaused(paused bool) State {
	m.paused.store(paused)
	state := m.State()
	go m.emitState(state)
	return state
}

// IsPaused 返回是否处于暂停抓取状态。
func (m *Manager) IsPaused() bool { return m.paused.load() }

// stateLocked 组装状态，调用方必须持有 m.mu。
func (m *Manager) stateLocked() State {
	return State{
		Running:   m.running,
		Addr:      net.JoinHostPort(m.cfg.Get().ListenHost, strconv.Itoa(m.cfg.Get().ProxyPort)),
		Paused:    m.paused.load(),
		Flows:     m.store.Len(),
		StartedAt: m.startedAt,
		LastError: m.lastErr,
	}
}

// watch 监听内核退出。只有当前正在使用的代理实例产生的错误才上报，
// 避免"停止后重启"时旧实例的错误把新实例状态改坏。
func (m *Manager) watch(p *mitm.Proxy, errCh <-chan error) {
	err := <-errCh
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return // 正常关闭
	}

	m.mu.Lock()
	if m.p != p {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.p = nil
	m.lastErr = readableStartError(err)
	state := m.stateLocked()
	m.mu.Unlock()

	applog.Errorf("代理运行期异常: %v", err)
	m.stopFlushLoop()
	m.emitState(state)
}

// shouldIntercept 返回内核的"是否解密该请求"判定函数。
// 未开启解密、命中跳过列表的域名、以及暂停状态一律走纯隧道，
// 内核的 directTransfer 会原样转发字节，不解密也不记录。
//
// 注意这里读的是运行时的 m.paused 原子量，而不是启动时的配置快照，
// 所以暂停/恢复对后续新连接立即生效，无需重启代理。
// 已建立的解密连接会保持到自然关闭，不影响现有网络访问。
func (m *Manager) shouldIntercept(cfg config.Config) func(*http.Request) bool {
	skip := cfg.SkipDecryptHosts
	return func(req *http.Request) bool {
		if m.paused.load() {
			return false
		}
		if !cfg.InterceptTLS {
			return false
		}
		host := strings.ToLower(req.URL.Hostname())
		for _, s := range skip {
			if host == s || strings.HasSuffix(host, "."+s) {
				return false
			}
		}
		return true
	}
}

// emitState 推送状态事件。
func (m *Manager) emitState(s State) {
	m.emitMu.RLock()
	emit := m.emit
	m.emitMu.RUnlock()
	if emit == nil {
		return
	}
	emit(events.EventProxyState, s)
}

// enqueue 把新会话放入待推送队列。
func (m *Manager) enqueue(b capture.FlowBrief) {
	m.queueMu.Lock()
	m.queue = append(m.queue, b)
	m.queueMu.Unlock()
}

// Publish 把外部产生的会话（如请求重放结果）交给批量推送队列。
// 之所以不直接 Emit：让所有会话更新走同一条通路，前端只需处理一种事件。
func (m *Manager) Publish(b capture.FlowBrief) {
	m.enqueue(b)
}

// enqueueFrames 把新帧按会话聚合进待推送队列。
func (m *Manager) enqueueFrames(b capture.FrameBatch) {
	m.frameMu.Lock()
	defer m.frameMu.Unlock()

	if m.frames == nil {
		m.frames = make(map[string]*capture.FrameBatch)
	}
	cur, ok := m.frames[b.FlowID]
	if !ok {
		cur = &capture.FrameBatch{FlowID: b.FlowID}
		m.frames[b.FlowID] = cur
		m.frameOrder = append(m.frameOrder, b.FlowID)
	}
	cur.Frames = append(cur.Frames, b.Frames...)
	cur.Total = b.Total
	cur.Dropped = b.Dropped
}

// startFlushLoop 启动批量推送循环。
func (m *Manager) startFlushLoop() {
	m.flushStop = make(chan struct{})
	m.flushDone = make(chan struct{})

	stop := m.flushStop
	done := m.flushDone
	go func() {
		defer close(done)
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				m.flush() // 退出前把剩余数据推完，避免最后几条丢失
				return
			case <-ticker.C:
				m.flush()
			}
		}
	}()
}

// stopFlushLoop 停止批量推送循环，可重复调用。
func (m *Manager) stopFlushLoop() {
	m.mu.Lock()
	stop, done := m.flushStop, m.flushDone
	m.flushStop, m.flushDone = nil, nil
	m.mu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// flush 将队列中的会话与帧一次性推给前端。
func (m *Manager) flush() {
	m.flushFlows()
	m.flushFrames()
}

// flushFlows 推送会话批次。
func (m *Manager) flushFlows() {
	m.queueMu.Lock()
	if len(m.queue) == 0 {
		m.queueMu.Unlock()
		return
	}
	batch := m.queue
	m.queue = nil
	m.queueMu.Unlock()

	m.emitMu.RLock()
	emit := m.emit
	m.emitMu.RUnlock()
	if emit == nil {
		return
	}
	emit(events.EventFlows, batch)
}

// flushFrames 推送帧批次。每个会话一条事件，避免一次事件携带过多数据。
func (m *Manager) flushFrames() {
	m.frameMu.Lock()
	if len(m.frameOrder) == 0 {
		m.frameMu.Unlock()
		return
	}
	order := m.frameOrder
	queued := m.frames
	m.frameOrder = nil
	m.frames = nil
	m.frameMu.Unlock()

	m.emitMu.RLock()
	emit := m.emit
	m.emitMu.RUnlock()
	if emit == nil {
		return
	}
	for _, id := range order {
		b := queued[id]
		if b == nil || len(b.Frames) == 0 {
			continue
		}
		emit(events.EventFrames, *b)
	}
}

// checkPortAvailable 通过实际监听探测端口是否可用，探测后立即释放。
// 存在极小的竞态窗口（探测完成到内核 bind 之间），但足以覆盖 99% 的冲突场景。
func checkPortAvailable(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("无法监听 %s，端口可能已被占用: %w", addr, err)
	}
	_ = ln.Close()
	return nil
}

// readableStartError 把内核的英文错误转成可操作的提示。
func readableStartError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "address already in use"):
		return "端口已被占用，请在设置中更换端口后重试"
	case strings.Contains(msg, "permission denied"):
		return "没有权限监听该端口（1024 以下端口需要管理员权限），请换用 1024 以上端口"
	case strings.Contains(msg, "cannot assign requested address"):
		return "监听地址不可用，请检查本机网络或改为 127.0.0.1"
	default:
		return "代理启动失败: " + msg
	}
}

// proxyDebugLevel 支持通过环境变量开启内核调试日志（排障用）。
func proxyDebugLevel() int {
	if os.Getenv("FLOWLENS_PROXY_DEBUG") == "1" {
		logrus.SetLevel(logrus.DebugLevel)
		return 1
	}
	logrus.SetLevel(logrus.WarnLevel)
	return 0
}

// atomicBool 是极简的原子布尔，避免为此引入额外依赖。
type atomicBool struct {
	mu sync.RWMutex
	v  bool
}

func (b *atomicBool) store(v bool) {
	b.mu.Lock()
	b.v = v
	b.mu.Unlock()
}

func (b *atomicBool) load() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.v
}
