// Package service 是前端与后端之间的绑定层。
//
// 职责边界（务必遵守）：
//   - 只做参数校验、调用领域模块、组装 DTO；
//   - 不实现抓包/证书/代理的业务细节；
//   - 不向内核类型泄露到前端（返回值必须是可 JSON 序列化的 DTO）。
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kerison/flowlens/internal/applog"
	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/certsrv"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/events"
	"github.com/kerison/flowlens/internal/maplocal"
	"github.com/kerison/flowlens/internal/mapremote"
	"github.com/kerison/flowlens/internal/proxy"
	"github.com/kerison/flowlens/internal/replay"
	"github.com/kerison/flowlens/internal/sysproxy"
	"github.com/kerison/flowlens/internal/trust"
)

// Version 由构建流程写入，默认值仅用于本地开发。
var Version = "0.1.0"

// Emitter 与 proxy.Emitter 同签名，便于 main 统一注入。
type Emitter func(event string, payload any)

// emitFn 是包级事件发送函数。
// 之所以不放成 App 的方法：绑定生成器会把服务上所有导出方法暴露给前端，
// 函数类型参数无法 JSON 序列化，会产生无效的绑定 API。
var emitFn Emitter

// SetEmitter 由 main 在应用实例就绪后调用。
func SetEmitter(e Emitter) { emitFn = e }

// App 是绑定给前端的根服务对象。
type App struct {
	cfg   *config.Manager
	caMgr *ca.Manager
	store *capture.Store
	prox  *proxy.Manager
	certs *certsrv.Server
	sys   *sysproxy.Manager
	// bootNote 是启动自愈的结论，随首屏返回给界面显示一次。
	bootNote string
}

// Deps 是服务层的依赖集合。
// 用具名结构体而不是超长参数列表：依赖继续增加时不会改坏所有调用点。
type Deps struct {
	Config     *config.Manager
	CA         *ca.Manager
	Store      *capture.Store
	Proxy      *proxy.Manager
	CertServer *certsrv.Server
	SysProxy   *sysproxy.Manager
	BootNote   string
}

// New 组装服务层依赖。
func New(d Deps) *App {
	return &App{
		cfg:      d.Config,
		caMgr:    d.CA,
		store:    d.Store,
		prox:     d.Proxy,
		certs:    d.CertServer,
		sys:      d.SysProxy,
		bootNote: d.BootNote,
	}
}

// Bootstrap 是界面首屏唯一入口，一次拿齐渲染所需的所有信息。
type Bootstrap struct {
	State      proxy.State     `json:"state"`
	CA         ca.Info         `json:"ca"`
	Config     config.Config   `json:"config"`
	Trust      trust.Status    `json:"trust"`
	Cert       CertServerInfo  `json:"cert"`
	SysProxy   sysproxy.Status `json:"sysProxy"`
	AppDir     string          `json:"appDir"`
	ConfigPath string          `json:"configPath"`
	LogDir     string          `json:"logDir"`
	Version    string          `json:"version"`
	Platform   string          `json:"platform"`
	ProxyURL   string          `json:"proxyUrl"` // 形如 http://127.0.0.1:8888，供复制
	LANIPs     []string        `json:"lanIps"`
	// SysProxyNote 是启动自愈的一句话结论（已自动恢复 / 需要人工恢复），只显示一次。
	SysProxyNote string `json:"sysProxyNote,omitempty"`
}

// CertServer 是证书分发服务的状态与可访问地址。
type CertServerInfo struct {
	Running bool     `json:"running"`
	Addr    string   `json:"addr"` // 监听地址，形如 0.0.0.0:8889
	URL     string   `json:"url"`  // 首选地址：局域网模式下是手机能访问的那个
	URLs    []string `json:"urls"` // 全部候选地址，含 127.0.0.1
	LAN     bool     `json:"lan"`  // 是否对局域网开放（监听 0.0.0.0）
	Port    int      `json:"port"`
	Error   string   `json:"error,omitempty"`
}

// localProxyAddress 把"监听所有网卡"翻译成"本机可填的代理地址"。
// 0.0.0.0 是监听语义，填进客户端的代理设置里并不能连接；127.0.0.1 才是。
func localProxyAddress(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}

// Bootstrap 返回首屏数据。
func (a *App) Bootstrap() Bootstrap {
	cfg := a.cfg.Get()
	return Bootstrap{
		State:        a.prox.State(),
		CA:           a.caMgr.Info(),
		Config:       cfg,
		Trust:        a.trustStatus(),
		Cert:         a.CertServer(),
		SysProxy:     a.sysProxyStatus(),
		AppDir:       a.cfg.Dir(),
		ConfigPath:   a.cfg.ConfigPath(),
		LogDir:       applog.Dir(),
		Version:      Version,
		Platform:     runtime.GOOS,
		// 顶栏/接入页展示的是"可直接填进客户端的地址"，不是监听通配地址
		ProxyURL:     "http://" + net.JoinHostPort(localProxyAddress(cfg.ListenHost), strconv.Itoa(cfg.ProxyPort)),
		LANIPs:       LANAddresses(),
		SysProxyNote: a.bootNote,
	}
}

// sysProxyStatus 返回系统代理接管状态；未装配时返回「不支持」，界面据此隐藏入口。
func (a *App) sysProxyStatus() sysproxy.Status {
	if a.sys == nil {
		return sysproxy.Status{Detail: "系统代理接管未启用"}
	}
	return a.sys.Status()
}

// SysProxyStatus 供界面刷新接管状态。
func (a *App) SysProxyStatus() sysproxy.Status { return a.sysProxyStatus() }

// EnableSysProxy 接管系统代理。
//
// 前置条件是代理已在运行：否则系统会指向一个没人监听的端口，
// 用户会在点下按钮的瞬间断网。
func (a *App) EnableSysProxy() (sysproxy.Result, error) {
	if a.sys == nil {
		return sysproxy.Result{Message: "系统代理接管未启用"}, errors.New("系统代理接管未启用")
	}
	if !a.prox.State().Running {
		// 返回完整可执行的提示：前端会原样展示，不再二次加工
		return sysproxy.Result{Message: "请先启动代理，再接管系统代理", Status: a.sys.Status()},
			errors.New("请先启动代理，再接管系统代理")
	}
	res, err := a.sys.Enable()
	if err != nil {
		applog.Warnf("接管系统代理失败：%v", err)
		return res, err
	}
	applog.Infof("已接管系统代理 -> %s", a.sys.Target())
	return res, nil
}

// DisableSysProxy 恢复系统代理原设置。失败时不要当作成功，否则用户会以为网络已恢复。
func (a *App) DisableSysProxy() (sysproxy.Result, error) {
	if a.sys == nil {
		return sysproxy.Result{Message: "系统代理接管未启用"}, errors.New("系统代理接管未启用")
	}
	res, err := a.sys.Disable()
	if err != nil {
		applog.Warnf("恢复系统代理失败：%v", err)
		return res, err
	}
	applog.Infof("已恢复系统代理：%s", res.Message)
	return res, nil
}

// trustStatus 检测本机信任状态。
// 每次调用都用当前 CA 文件重建 trust.Manager：重新生成根证书后不会拿着旧证书做检测。
func (a *App) trustStatus() trust.Status {
	m, err := trust.New(a.caMgr.CertPath())
	if err != nil {
		return trust.Status{Detail: "证书读取失败：" + err.Error()}
	}
	return m.Status()
}

// TrustStatus 返回本机信任状态，供界面刷新。
func (a *App) TrustStatus() trust.Status { return a.trustStatus() }

// InstallTrust 把根证书装进本机信任库。
// elevated=false 用用户级信任库（无需密码）；true 用系统级（弹授权框）。
// 界面必须先展示 TrustStatus().Command 并让用户确认，再调用本方法。
func (a *App) InstallTrust(elevated bool) (trust.Result, error) {
	m, err := trust.New(a.caMgr.CertPath())
	if err != nil {
		return trust.Result{}, fmt.Errorf("读取证书失败: %w", err)
	}
	var res trust.Result
	if elevated {
		res, err = m.InstallElevated()
	} else {
		res, err = m.Install()
	}
	if err != nil {
		applog.Warnf("安装根证书失败（elevated=%v）：%v", elevated, err)
		return res, err
	}
	applog.Infof("根证书已安装到本机信任库：%s", res.Message)
	return res, nil
}

// UninstallTrust 从本机信任库移除根证书。
func (a *App) UninstallTrust() (trust.Result, error) {
	m, err := trust.New(a.caMgr.CertPath())
	if err != nil {
		return trust.Result{}, fmt.Errorf("读取证书失败: %w", err)
	}
	res, err := m.Uninstall()
	if err != nil {
		applog.Warnf("移除根证书失败：%v", err)
		return res, err
	}
	applog.Infof("根证书已移除：%s", res.Message)
	return res, nil
}

// CertServer 返回证书分发服务的当前状态与可访问地址。
// 只返回"能连上"的地址：局域网模式下优先给手机可用的 IP。
func (a *App) CertServer() CertServerInfo {
	cfg := a.cfg.Get()
	out := CertServerInfo{Port: cfg.CertPort, LAN: isLANMode(cfg.ListenHost)}
	if a.certs == nil {
		out.Error = "证书服务未初始化"
		return out
	}
	out.Running = a.certs.Running()
	out.Addr = a.certs.Addr()
	if !out.Running {
		return out
	}

	port := strconv.Itoa(cfg.CertPort)
	if out.LAN {
		for _, ip := range LANAddresses() {
			out.URLs = append(out.URLs, "http://"+net.JoinHostPort(ip, port))
		}
		if len(out.URLs) > 0 {
			out.URL = out.URLs[0] // 局域网优先，扫码才有意义
		}
	}
	local := "http://127.0.0.1:" + port
	if out.URL == "" {
		out.URL = local
	}
	out.URLs = append(out.URLs, local)
	return out
}

// RestartCertServer 按当前配置与证书重启证书分发服务。
// 重新生成根证书、修改端口或切换监听地址后都要调用，否则分发的还是旧证书/旧地址。
func (a *App) RestartCertServer() (CertServerInfo, error) {
	if a.certs == nil {
		return CertServerInfo{}, errors.New("证书服务未初始化")
	}
	if err := a.certs.Stop(); err != nil {
		applog.Warnf("停止证书服务失败：%v", err)
	}
	cfg := a.cfg.Get()
	info := a.caMgr.Info()
	a.certs = certsrv.New(certsrv.Options{
		Host:        cfg.ListenHost,
		Port:        cfg.CertPort,
		CertPEM:     []byte(info.PEM),
		CertDER:     a.caMgr.GetRootCA().Raw,
		CommonName:  info.CommonName,
		Fingerprint: info.Fingerprint,
		NotAfter:    info.NotAfter,
		Version:     Version,
	})
	if _, err := a.certs.Start(); err != nil {
		applog.Warnf("启动证书服务失败：%v", err)
		st := a.CertServer()
		st.Error = err.Error()
		return st, err
	}
	return a.CertServer(), nil
}

// SelfTest 通过代理发起一次需校验证书的 HTTPS 请求，验证「代理→解密→信任」整条链路。
func (a *App) SelfTest() proxy.SelfTestResult {
	res := a.prox.SelfTest("")
	if res.OK {
		applog.Infof("自检通过：%s", res.Message)
	} else {
		applog.Warnf("自检未通过：%s（%s）", res.Message, res.Hint)
	}
	return res
}

// isLANMode 判断监听地址是否对局域网开放。
// 排除 127.0.0.1 / localhost / ::1 这些只有本机能访问的地址。
func isLANMode(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return false
	}
	return true
}

// StartProxy 启动代理。
func (a *App) StartProxy() (proxy.State, error) {
	state, err := a.prox.Start()
	if err != nil {
		return state, err
	}
	return state, nil
}

// StopProxy 停止代理。
//
// 顺序很关键：先摘掉系统代理再停监听。
// 反过来的话系统会指向一个已关闭的端口，用户立刻表现为"断网"；
// 而摘不掉系统代理时宁可拒绝停止，也不要把用户丢在没有网络的处境里。
func (a *App) StopProxy() (proxy.State, error) {
	if a.sys != nil {
		if st := a.sys.Status(); st.Enabled {
			if _, err := a.sys.Disable(); err != nil {
				return a.prox.State(), fmt.Errorf(
					"系统代理仍指向 观流（%s），已取消停止以避免断网；请先在「设置 → 系统代理」里恢复，或授权后重试：%w",
					st.Addr, err)
			}
		}
	}
	if err := a.prox.Stop(); err != nil {
		return a.prox.State(), err
	}
	return a.prox.State(), nil
}

// RestartProxy 在运行中重启代理，用于配置变更后立即生效。
// 必须先走 StopProxy：它负责在停监听之前把系统代理摘掉。
func (a *App) RestartProxy() (proxy.State, error) {
	wasRunning := a.prox.State().Running
	if wasRunning {
		if _, err := a.StopProxy(); err != nil {
			return a.prox.State(), fmt.Errorf("重启前停止代理失败: %w", err)
		}
	}
	return a.prox.Start()
}

// SetPaused 暂停或恢复抓取。
func (a *App) SetPaused(paused bool) proxy.State {
	return a.prox.SetPaused(paused)
}

// ProxyState 返回当前代理状态。
func (a *App) ProxyState() proxy.State { return a.prox.State() }

// ListFlows 返回 seq 大于 since 的会话。首次传 0 取全量。
func (a *App) ListFlows(since uint64) []capture.FlowBrief {
	return a.store.ListSince(since)
}

// maxBodySearchHits 是响应体搜索单次返回的命中上限。
// 关键字太宽泛时（如 "a"）可能命中上千条，全量返回会让序列化与渲染卡顿；
// 截断后界面已足够提示"命中太多，请换更精确的关键字"。
const maxBodySearchHits = 2000

// SearchBodies 在内存保留的响应体文本里搜索关键字，返回命中的会话（时间正序）。
//
// 搜索对象是详情页看到的解码文本（ResBody.Preview，已按字符集解码），
// 因此 GBK/UTF-16 等编码的中文也能直接搜；二进制内容天然不参与。
// 大小写不敏感；超过上限截断。找不到或关键字为空返回空切片。
func (a *App) SearchBodies(keyword string) []capture.FlowBrief {
	return searchBodyFlows(a.store.GetAll(), keyword, maxBodySearchHits)
}

// searchBodyFlows 是 SearchBodies 的纯函数实现，便于测试。
func searchBodyFlows(flows []*capture.FlowDetail, keyword string, limit int) []capture.FlowBrief {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	if kw == "" {
		return nil
	}
	out := make([]capture.FlowBrief, 0, 8)
	for _, d := range flows {
		if d == nil || d.ResBody == nil || d.ResBody.Preview == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(d.ResBody.Preview), kw) {
			continue
		}
		out = append(out, d.FlowBrief)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// FlowDetail 按 ID 返回会话详情。
func (a *App) FlowDetail(id string) (*capture.FlowDetail, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("会话 ID 不能为空")
	}
	d, ok := a.store.Get(id)
	if !ok {
		// 会话可能已被淘汰，提示要明确，避免用户以为程序出错
		return nil, errors.New("该会话已不在内存中（可能已被新会话挤出），请重新触发一次请求")
	}
	return d, nil
}

// ClearFlows 清空所有会话。
func (a *App) ClearFlows() error {
	a.store.Clear()
	if emitFn != nil {
		emitFn(events.EventFlowsCleared, nil)
	}
	applog.Infof("已清空会话")
	return nil
}

// DeleteFlows 按 ID 删除会话，返回实际删除数量。
// 与 ClearFlows 分开是因为删除是常用操作，而清空是破坏性操作，二者的确认强度不同。
func (a *App) DeleteFlows(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, errors.New("没有选中任何会话")
	}
	deleted := a.store.Delete(ids)
	if len(deleted) == 0 {
		return 0, nil
	}
	if emitFn != nil {
		emitFn(events.EventFlowsDeleted, deleted)
	}
	applog.Infof("已删除 %d 条会话", len(deleted))
	return len(deleted), nil
}

// Frames 返回指定会话已捕获的 WebSocket/SSE 帧。
// 与详情分开拉取：长连接的帧可能上千条，塞进详情会让每次点开都卡一下。
func (a *App) Frames(id string) (capture.FrameBatch, error) {
	if strings.TrimSpace(id) == "" {
		return capture.FrameBatch{}, errors.New("会话 ID 不能为空")
	}
	frames, dropped := a.store.Frames(id)
	if frames == nil {
		frames = []capture.Frame{}
	}
	return capture.FrameBatch{
		FlowID:  id,
		Frames:  frames,
		Total:   len(frames),
		Dropped: dropped,
	}, nil
}

// HARExport 是导出结果，供界面提示用户"文件在哪、导了多少条"。
type HARExport struct {
	Path    string `json:"path"`
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
}

// ExportHAR 导出 HAR 文件。
// ids 为空表示导出全部会话；否则只导出指定的那些（用于"导出选中"）。
// target 为空时写入系统下载目录并自动命名；非空时写到用户选定的路径
// （路径由界面的原生保存对话框取得，后端只负责落盘与校验）。
func (a *App) ExportHAR(ids []string, target string) (HARExport, error) {
	var flows []*capture.FlowDetail
	if len(ids) == 0 {
		flows = a.store.GetAll()
	} else {
		flows = a.store.Pick(ids)
	}
	if len(flows) == 0 {
		return HARExport{}, errors.New("没有可导出的会话")
	}

	data, err := capture.BuildHAR(flows, Version)
	if err != nil {
		return HARExport{}, fmt.Errorf("生成 HAR 失败: %w", err)
	}

	path, err := writeExportFile(data, "har", target)
	if err != nil {
		return HARExport{}, err
	}
	applog.Infof("已导出 HAR：%s（%d 条）", path, len(flows))
	return HARExport{Path: path, Entries: len(flows), Bytes: int64(len(data))}, nil
}

// ExportJSON 导出 观流 自有 JSON 快照（字段与 HAR 不同，见 capture/json.go）。
// 用途与 HAR 互补：HAR 给外部工具，JSON 给回归对比与脚本加工。
// target 语义同 ExportHAR：空则落到下载目录，非空则用用户选定的路径。
func (a *App) ExportJSON(ids []string, target string) (HARExport, error) {
	var flows []*capture.FlowDetail
	if len(ids) == 0 {
		flows = a.store.GetAll()
	} else {
		flows = a.store.Pick(ids)
	}
	if len(flows) == 0 {
		return HARExport{}, errors.New("没有可导出的会话")
	}

	data, err := capture.BuildJSON(flows, Version)
	if err != nil {
		return HARExport{}, fmt.Errorf("生成 JSON 失败: %w", err)
	}
	path, err := writeExportFile(data, "json", target)
	if err != nil {
		return HARExport{}, err
	}
	applog.Infof("已导出 JSON：%s（%d 条）", path, len(flows))
	return HARExport{Path: path, Entries: len(flows), Bytes: int64(len(data))}, nil
}

// writeExportFile 落盘导出内容。
// target 为界面保存对话框返回的路径：非空时按它写入（缺扩展名则补上），
// 空则退回下载目录并生成 flowlens-<时间戳>.<ext>，同一秒内连续导出加序号而不是覆盖。
func writeExportFile(data []byte, ext, target string) (string, error) {
	if target != "" {
		path, err := resolveTargetPath(target, ext)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", fmt.Errorf("写入导出文件失败: %w", err)
		}
		return path, nil
	}

	dir := downloadDir()
	if dir == "" {
		dir = os.TempDir()
	}
	stamp := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, "flowlens-"+stamp+"."+ext)

	for i := 1; i < 100; i++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("flowlens-%s-%d.%s", stamp, i, ext))
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("写入导出文件失败: %w", err)
	}
	return path, nil
}

// resolveTargetPath 规整用户选定的导出路径。
// 只补"没有扩展名"的情况：用户明确写成 trace.txt 就尊重原样，
// 否则会得到 trace.txt.json 这种谁都不想看到的文件名。
func resolveTargetPath(target, ext string) (string, error) {
	path := filepath.Clean(target)

	// 先判目录再补扩展名：否则用户选中一个目录时会得到 "<目录>.json" 这种离谱文件名
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return "", fmt.Errorf("保存路径是一个目录: %s", path)
	}
	if filepath.Ext(path) == "" {
		path += "." + ext
	}

	st, err := os.Stat(filepath.Dir(path))
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("保存目录不存在: %s", filepath.Dir(path))
	}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return "", fmt.Errorf("保存路径是一个目录: %s", path)
	}
	return path, nil
}

// downloadDir 返回系统下载目录，不存在时返回空字符串。
func downloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, "Downloads")
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir
	}
	return ""
}

// ReplayFlow 重放指定会话，结果作为一条新会话入库并推送。
func (a *App) ReplayFlow(id string) (*capture.FlowDetail, error) {
	src, ok := a.store.Get(id)
	if !ok {
		return nil, errors.New("该会话已不在内存中，无法重放")
	}
	cfg := a.cfg.Get()

	rp, err := replay.New(replay.Options{
		UpstreamProxy: cfg.UpstreamProxy,
		Insecure:      cfg.InsecureUpstream,
		MaxBodyBytes:  cfg.MaxBodyBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化重放失败: %w", err)
	}

	detail, err := rp.Do(src)
	if err != nil {
		applog.Warnf("重放 %s 失败: %v", id, err)
		return nil, err
	}

	brief, _ := a.store.Add(detail)
	a.prox.Publish(brief)
	applog.Infof("已重放 %s %s://%s%s", src.Method, src.Scheme, src.Host, src.Path)
	return detail, nil
}

// HeaderPair 是一个请求头键值对。
// 用有序数组而非 map：Cookie、Accept 这类同名多项会在 map 里丢掉顺序与重复项，
// 而重放要尽可能贴近原始请求。
type HeaderPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ReplayEdit 是"重放并编辑"的输入。字段为空表示沿用源会话的对应部分。
type ReplayEdit struct {
	Method  string       `json:"method"`
	URL     string       `json:"url"`
	Headers []HeaderPair `json:"headers"`
	Body    *string      `json:"body"`
}

// ReplayFlowEdit 按用户编辑后的内容重放，结果作为一条新会话入库并推送。
func (a *App) ReplayFlowEdit(id string, edit ReplayEdit) (*capture.FlowDetail, error) {
	src, ok := a.store.Get(id)
	if !ok {
		return nil, errors.New("该会话已不在内存中，无法重放")
	}
	cfg := a.cfg.Get()

	rp, err := replay.New(replay.Options{
		UpstreamProxy: cfg.UpstreamProxy,
		Insecure:      cfg.InsecureUpstream,
		MaxBodyBytes:  cfg.MaxBodyBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化重放失败: %w", err)
	}

	ov := &replay.Override{
		Method: edit.Method,
		URL:    edit.URL,
		Body:   edit.Body,
	}
	if edit.Headers != nil {
		h := make(http.Header, len(edit.Headers))
		for _, p := range edit.Headers {
			name := strings.TrimSpace(p.Name)
			if name == "" {
				continue // 允许界面留空行，不用报错打断
			}
			h.Add(name, p.Value)
		}
		ov.Headers = h
	}

	detail, err := rp.DoWith(src, ov)
	if err != nil {
		applog.Warnf("编辑重放 %s 失败: %v", id, err)
		return nil, err
	}

	brief, _ := a.store.Add(detail)
	a.prox.Publish(brief)
	applog.Infof("已按编辑内容重放 %s %s", detail.Method, detail.URL())
	return detail, nil
}

// CAInfo 返回根证书信息。
func (a *App) CAInfo() ca.Info { return a.caMgr.Info() }

// RegenerateCA 重新生成根证书。
// 调用方必须先卸载旧证书，否则客户端会因证书不匹配而报错，因此这里二次确认由界面负责。
func (a *App) RegenerateCA() (ca.Info, error) {
	info, err := a.caMgr.Regenerate()
	if err != nil {
		return ca.Info{}, fmt.Errorf("重新生成证书失败: %w", err)
	}
	// 证书换了，下载页还在发旧证书的话，手机装完依然抓不到——必须同步重启
	if a.certs != nil {
		if _, err := a.RestartCertServer(); err != nil {
			applog.Warnf("重新生成证书后重启证书服务失败：%v", err)
		}
	}
	applog.Warnf("已重新生成根证书，指纹 %s", info.Fingerprint)
	return info, nil
}

// CAExport 是证书导出结果。
type CAExport struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// ExportCA 把根证书连同私钥导出为 .p12，写入系统下载目录。
//
// 这是应用中唯一会把私钥写到应用数据目录之外的操作，因此：
//   - 必须带密码（至少 8 位），由 ui 层做二次确认后才调用；
//   - 文件权限 0600，且不覆盖已有同名文件，避免悄悄顶掉用户之前导出的副本；
//   - 日志只记录路径与大小，绝不记录密码。
func (a *App) ExportCA(password string) (CAExport, error) {
	data, err := a.caMgr.ExportPKCS12(password)
	if err != nil {
		return CAExport{}, err
	}

	dir := downloadDir()
	if dir == "" {
		dir = os.TempDir()
	}
	name := "flowlens-ca.p12"
	path := filepath.Join(dir, name)
	for i := 1; i < 100; i++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("flowlens-ca-%d.p12", i))
	}

	// 私钥文件用 0600：同机其他用户不应读到，即使用户把它放在共享目录
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return CAExport{}, fmt.Errorf("写入证书文件失败: %w", err)
	}
	applog.Warnf("已导出含私钥的证书文件：%s（%d 字节），请妥善保管", path, len(data))
	return CAExport{Path: path, Bytes: int64(len(data))}, nil
}

// GetConfig 返回当前配置。
func (a *App) GetConfig() config.Config { return a.cfg.Get() }

// SaveConfig 保存配置。若代理正在运行且网络参数变化，需要调用 RestartProxy 生效。
func (a *App) SaveConfig(next config.Config) (config.Config, error) {
	before := a.cfg.Get()
	// 这里必须硬拦：config.Validate 对"开了认证但没密码"是静默关闭认证，
	// 若不在保存路径上报错，用户会以为认证已生效，实际代理对局域网敞开。
	if next.RequireAuth && strings.TrimSpace(next.AuthPassword) == "" {
		return before, errors.New("已勾选「代理认证」但没有填写密码：请设置密码，或取消勾选")
	}
	// Map Local 同理：保存时规则匹配不上/文件读不到，用户会以为 mock 已生效
	if err := maplocal.ValidateRules(next.MapLocalRules); err != nil {
		return before, err
	}
	if err := mapremote.ValidateRules(next.MapRemoteRules); err != nil {
		return before, err
	}
	if err := a.cfg.Update(func(c *config.Config) error {
		*c = next
		return nil
	}); err != nil {
		return before, fmt.Errorf("保存配置失败: %w", err)
	}
	return a.afterConfigSaved(before), nil
}

// afterConfigSaved 配置落盘后的联动：调整存储容量、按需重启证书服务与系统代理改指。
// SaveConfig 与 ImportConfig 共用，保证两条路径行为一致。
func (a *App) afterConfigSaved(before config.Config) config.Config {
	saved := a.cfg.Get()
	a.store.Resize(saved.MaxFlows)

	// 证书服务的端口/监听地址变了就要重开，否则界面上的二维码指向旧地址
	if a.certs != nil && (saved.CertPort != before.CertPort || saved.ListenHost != before.ListenHost) {
		if _, err := a.RestartCertServer(); err != nil {
			applog.Warnf("配置已保存，但证书服务未能按新地址重启：%v", err)
		}
	}

	// 代理端口变了且系统代理正被接管：必须改指新端口，
	// 否则用户"改完端口就上不了网"，而原因看起来和端口毫无关系。
	if a.sys != nil && saved.ProxyPort != before.ProxyPort {
		if _, err := a.sys.Repoint(); err != nil {
			applog.Warnf("系统代理改指新端口失败：%v", err)
		}
	}

	applog.Infof("配置已更新")
	return saved
}

// configExportFormat 是配置导出文件的格式标识与版本。
// 包一层而不是裸配置：将来字段大改时，导入逻辑能靠版本号做兼容判断。
type configExport struct {
	Format string        `json:"flowlens.config"`
	Config config.Config `json:"config"`
}

// ConfigExport 是配置导出结果。
type ConfigExport struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// ExportConfig 把当前配置导出为 JSON 文件。target 语义同 ExportHAR。
func (a *App) ExportConfig(target string) (ConfigExport, error) {
	data, err := json.MarshalIndent(configExport{
		Format: "flowlens.config/1",
		Config: a.cfg.Get(),
	}, "", "  ")
	if err != nil {
		return ConfigExport{}, fmt.Errorf("序列化配置失败: %w", err)
	}
	path, err := writeExportFile(data, "json", target)
	if err != nil {
		return ConfigExport{}, err
	}
	applog.Infof("已导出配置：%s", path)
	return ConfigExport{Path: path, Bytes: int64(len(data))}, nil
}

// maxImportConfigSize 限制导入配置的大小。配置撑死几百 KB，
// 超过这个值几乎可以肯定用户选错了文件。
const maxImportConfigSize = 1 << 20

// ImportConfig 从文件导入配置并立即生效。接受导出格式（带包装）与裸配置两种。
func (a *App) ImportConfig(path string) (config.Config, error) {
	if strings.TrimSpace(path) == "" {
		return config.Config{}, errors.New("请选择要导入的配置文件")
	}
	info, err := os.Stat(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("无法读取文件：%w", err)
	}
	if info.IsDir() {
		return config.Config{}, errors.New("选择的是目录，请选择配置 JSON 文件")
	}
	if info.Size() > maxImportConfigSize {
		return config.Config{}, errors.New("文件过大，不是 观流 配置文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("读取文件失败：%w", err)
	}

	var next config.Config
	var wrapped configExport
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Format != "" {
		next = wrapped.Config
	} else if err := json.Unmarshal(data, &next); err != nil {
		return config.Config{}, fmt.Errorf("文件不是合法的 观流 配置：%w", err)
	}

	// 与 SaveConfig 走同一条校验与联动路径：认证密码、规则、容量、证书服务全部一致
	before := a.cfg.Get()
	if next.RequireAuth && strings.TrimSpace(next.AuthPassword) == "" {
		return before, errors.New("导入的配置勾选了「代理认证」但没有密码：为安全起见已拒绝，请在导出端补齐密码后重试")
	}
	if err := maplocal.ValidateRules(next.MapLocalRules); err != nil {
		return before, fmt.Errorf("导入的配置存在问题：%w", err)
	}
	if err := mapremote.ValidateRules(next.MapRemoteRules); err != nil {
		return before, fmt.Errorf("导入的配置存在问题：%w", err)
	}
	if err := a.cfg.Update(func(c *config.Config) error {
		*c = next
		return nil
	}); err != nil {
		return before, fmt.Errorf("应用导入配置失败: %w", err)
	}
	saved := a.afterConfigSaved(before)
	applog.Infof("已从 %s 导入配置", path)
	return saved, nil
}

// ResolveBreakpoint 把用户对断点的处置投递给代理内核。
// 返回 false 表示该断点已不存在（超时自动放行或代理已停止）。
func (a *App) ResolveBreakpoint(holdKey string, decision proxy.BreakpointDecision) (bool, error) {
	if strings.TrimSpace(holdKey) == "" {
		return false, errors.New("断点标识不能为空")
	}
	if err := decision.Validate(); err != nil {
		return false, err
	}
	if !a.prox.ResolveBreakpoint(holdKey, decision) {
		return false, errors.New("该断点已失效（可能已超时自动放行），请刷新后重试")
	}
	return true, nil
}

// LANAddresses 返回本机可用于局域网访问的 IPv4 地址。
// 虚拟机/容器/隧道网卡的名称特征。这些地址虽然能被本机访问，
// 但手机扫出来连不上（ Guest NAT / Host-Only 网络），放进接入向导只会让人试错。
// 大小写不敏感，兼容 Windows 上 localized 的 "VMware Virtual Ethernet Adapter" 等。
var virtualInterfaceMarks = []string{
	"vmnet", "vboxnet", "virtualbox", "vmware", "parallels", "host-guest",
	"vethernet", "docker", "wsl", "utun", "awdl", "llw", "tun", "tap",
	"tailscale", "zerotier", "hamachi", "ppp", "gif", "sit",
}

// 常见的虚拟机 Host-Only 网段：即使网卡名字被改过也不该出现在手机接入列表里。
// 只列专用于虚拟化的固定网段，不扩大到整个 192.168/172.16 ——那些是真实路由器常用地址。
var virtualNetworkPrefixes = []net.IPNet{
	{IP: net.IPv4(10, 211, 55, 0), Mask: net.CIDRMask(24, 32)},  // Parallels Shared
	{IP: net.IPv4(10, 37, 129, 0), Mask: net.CIDRMask(24, 32)},  // Parallels Host-Guest
	{IP: net.IPv4(192, 168, 56, 0), Mask: net.CIDRMask(24, 32)}, // VirtualBox Host-Only
}

func isVirtualInterface(name string) bool {
	n := strings.ToLower(name)
	for _, mark := range virtualInterfaceMarks {
		if strings.Contains(n, mark) {
			return true
		}
	}
	return false
}

func isVirtualAddress(ip net.IP) bool {
	for _, prefix := range virtualNetworkPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// LANAddresses 返回真实可用于手机接入的局域网 IPv4（排序后）。
//
// 过滤原则：只展示"同网段的手机真能连到"的地址。虚拟网卡地址（Parallels
// 10.211.55.x、VirtualBox Host-Only 192.168.56.x 等）和隧道/容器接口一律排除；
// 看名字认不出来的虚拟化网段再用固定网段兜底。宁可少列，也不要让人照着
// 一个连不上的地址排错半天。
func LANAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if isVirtualAddress(ip) {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}
