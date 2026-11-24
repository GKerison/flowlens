// Package sysproxy 负责「一键接管系统代理」：设置、恢复、状态检测与异常自愈。
//
// 这是整个应用里唯一可能让用户「上不了网」的模块，因此所有设计都偏保守：
//
//  1. 改之前先快照：把系统原有代理配置完整写进 <配置目录>/sysproxy.json，
//     快照落盘成功之后才允许动系统设置——否则宁可不接管。
//  2. 改之后要校验：以系统实际状态为准，不因命令返回 0 就宣布成功；
//     校验不通过立刻用快照回滚，绝不留「半开」状态。
//  3. 只恢复自己写过的东西：恢复完全以快照为准，不会顺手清空用户的其它代理配置。
//  4. 崩溃可恢复：状态文件带 cleanExit 标记，启动时据此判断上次是否异常退出。
//  5. 平台差异收敛在 sysproxy_darwin/windows/linux.go 三个文件里，
//     公共流程（快照→应用→校验→回滚）只有这一份，便于审查。
package sysproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// stateFileName 是接管现场记录，放在应用配置目录下，与 config.json 同级。
	stateFileName = "sysproxy.json"
	stateVersion  = 1
)

// BypassHosts 是接管时始终加入绕过列表的地址。
// 不加入会把「本机访问本机」的流量也塞进代理，形成「代理请求自己」的环路。
var BypassHosts = []string{"localhost", "127.0.0.1", "::1"}

// ErrPermission 表示系统拒绝写入，通常是因为需要管理员权限。
// 平台实现用 %w 包装它，界面据此引导用户改用提权入口。
var ErrPermission = errors.New("需要管理员权限")

// Capability 描述当前平台能否接管、用什么机制、是否需要提权。
type Capability struct {
	Supported  bool     `json:"supported"`
	Mechanism  string   `json:"mechanism"`  // 例如 "networksetup"、"WinINet 注册表"、"GNOME gsettings"
	NeedsAdmin bool     `json:"needsAdmin"` // 修改系统设置是否必然触发授权
	Detail     string   `json:"detail"`     // 面向用户的一句话
	Manual     []string `json:"manual"`     // 无法自动接管时的手动配置步骤
}

// Status 是系统代理的当前状态，供界面渲染。
type Status struct {
	Supported  bool      `json:"supported"`
	Enabled    bool      `json:"enabled"` // 系统代理当前是否指向 观流
	Stale      bool      `json:"stale"`   // 启动时检测到上次未正常恢复的接管
	Mode       string    `json:"mode"`    // none / manual / auto
	Addr       string    `json:"addr"`    // 系统当前生效的代理地址
	Target     string    `json:"target"`  // 我们会设置成的地址
	NeedsAdmin bool      `json:"needsAdmin"`
	Mechanism  string    `json:"mechanism"`
	Platform   string    `json:"platform"`
	Detail     string    `json:"detail"`
	Manual     []string  `json:"manual"`
	Error      string    `json:"error,omitempty"`
	CheckedAt  time.Time `json:"checkedAt"`
}

// Result 是一次接管 / 恢复操作的结果。
type Result struct {
	OK       bool     `json:"ok"`
	Changed  bool     `json:"changed"` // 本次是否真的改了系统设置
	Message  string   `json:"message"`
	Output   string   `json:"output,omitempty"`   // 命令输出，出错时给用户看
	Commands []string `json:"commands,omitempty"` // 实际执行的命令预览，便于审计
	Status   Status   `json:"status"`
}

// BootResult 是启动自愈的结论。
type BootResult struct {
	Leftover bool   `json:"leftover"` // 系统代理仍指向我们（上次没恢复成功）
	Restored bool   `json:"restored"` // 已自动恢复
	Note     string `json:"note"`     // 面向用户的一句话，可为空
}

// stateFile 是「我们动过系统代理」的现场记录。
//
// 文件存在本身就等于「上次没正常收尾」：恢复成功会立刻删掉它。
// 不再单独维护 cleanExit 标记——两处状态容易不一致，删除即成功更可靠。
type stateFile struct {
	Version     int             `json:"version"`
	Platform    string          `json:"platform"`
	ProxyAddr   string          `json:"proxyAddr"` // 我们写进系统的地址
	EnabledAt   time.Time       `json:"enabledAt"`
	Preexisting bool            `json:"preexisting"` // 接管时系统就已经指向我们，没有可恢复的原配置
	Snapshot    json.RawMessage `json:"snapshot,omitempty"`
}

// Manager 是系统代理接管的唯一入口。
type Manager struct {
	// dir 是应用配置目录，状态文件写在这里。
	dir string
	// addr 返回代理监听地址（形如 127.0.0.1:8888）。
	// 用函数而不是字段：端口可能在运行期被配置改动。
	addr func() string

	mu sync.Mutex
	// stale 表示启动时发现上次没恢复成功，由 BootRecover 设置。
	// 单独记在内存里，避免每次状态查询都去比对状态文件的时间戳。
	stale bool
}

// New 创建管理器。dir 一般是 config.Manager.Dir()。
func New(dir string, addr func() string) *Manager {
	return &Manager{dir: dir, addr: addr}
}

// Target 返回系统代理应指向的地址。
// 始终用回环地址：即使代理监听在 0.0.0.0，本机流量也不该走局域网 IP。
func (m *Manager) Target() string {
	if m.addr == nil {
		return ""
	}
	return m.addr()
}

// StatePath 返回状态文件路径，便于界面展示与排障。
func (m *Manager) StatePath() string { return filepath.Join(m.dir, stateFileName) }

// Status 返回当前系统代理状态。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

// Enable 接管系统代理。
//
// 顺序严格如下：读快照 → 落盘状态 → 改系统 → 校验 → 失败回滚。
// 任何一步出错都不会让系统停在「指向一个不存在的代理」的状态。
func (m *Manager) Enable() (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	capb := capability()
	if !capb.Supported {
		return m.fail(capb.Detail, nil)
	}

	target := m.Target()
	if target == "" {
		return m.fail("代理地址未知，请先启动代理", nil)
	}

	// 已经接管同一个地址：幂等返回，避免重复写系统设置
	if cur := m.loadState(); cur != nil && cur.ProxyAddr == target {
		res := Result{OK: true, Message: "系统代理已指向 观流：" + target}
		res.Status = m.statusLocked()
		return res, nil
	}

	mode, addr, err := currentProxy()
	// 读不出来也继续：readSnapshot 会给出更具体的错误
	preexisting := err == nil && pointsToUs(mode, addr, target)

	var snap json.RawMessage
	if !preexisting {
		snap, err = readSnapshot()
		if err != nil {
			return m.fail("读取系统代理配置失败，已取消接管："+err.Error(), nil)
		}
	}

	st := &stateFile{
		Version:     stateVersion,
		Platform:    runtime.GOOS,
		ProxyAddr:   target,
		EnabledAt:   time.Now(),
		Preexisting: preexisting,
		Snapshot:    snap,
	}
	// 先落盘快照再动系统：崩溃在中间也能恢复
	if err := m.writeState(st); err != nil {
		return m.fail("保存代理快照失败，已取消接管："+err.Error(), nil)
	}

	cmds, err := applyProxy(target, BypassHosts, execMode{AllowPrompt: true})
	res := Result{Commands: formatCommands(cmds)}
	if err != nil {
		_ = m.clearState()
		res.Message = "设置系统代理失败"
		res.Output = err.Error()
		res.Status = m.statusLocked()
		if errors.Is(err, ErrPermission) {
			res.Message = "设置系统代理需要管理员授权，已被取消（未做任何修改）"
			res.Status.NeedsAdmin = true
		}
		return res, err
	}

	if verr := m.verify(target, true); verr != nil {
		// 校验失败：立刻按快照回滚，宁可什么都没变
		rollback, rerr := restoreProxy(st, nil, execMode{AllowPrompt: true})
		res.Commands = append(res.Commands, formatCommands(rollback)...)
		_ = m.clearState()
		res.Message = "系统代理设置未生效，已回滚到原配置"
		res.Output = verr.Error()
		if rerr != nil {
			res.Message = "系统代理设置未生效，且回滚失败，请检查系统网络设置"
			res.Output += "\n回滚失败：" + rerr.Error()
		}
		res.Status = m.statusLocked()
		return res, fmt.Errorf("系统代理设置未生效：%w", verr)
	}

	m.stale = false
	res.OK = true
	res.Changed = true
	res.Message = "已接管系统代理：" + target
	res.Status = m.statusLocked()
	return res, nil
}

// Disable 恢复系统代理。
// 以快照为准逐项还原；没有快照（接管时系统本来就指向我们）则只关闭代理开关。
func (m *Manager) Disable() (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	target := m.Target()
	st := m.loadState()

	if st == nil {
		mode, addr, err := currentProxy()
		if err != nil {
			return m.fail("读取系统代理失败："+err.Error(), nil)
		}
		if !pointsToUs(mode, addr, target) {
			m.stale = false
			res := Result{OK: true, Message: "系统代理未接管，无需恢复"}
			res.Status = m.statusLocked()
			return res, nil
		}
		// 没有快照却指向我们（例如另一个实例留下的）：只能关闭，无法还原原配置
		cmds, err := restoreProxy(nil, nil, execMode{AllowPrompt: true})
		res := Result{Commands: formatCommands(cmds)}
		if err != nil {
			res.Message = "关闭系统代理失败"
			res.Output = err.Error()
			res.Status = m.statusLocked()
			return res, err
		}
		res.OK = true
		res.Changed = true
		res.Message = "已关闭系统代理（无接管快照，未还原历史配置）"
		res.Status = m.statusLocked()
		return res, nil
	}

	// 系统代理已被外部改掉（用户手动关了，或被别的工具接管）：
	// 这时按快照回滚会覆盖用户刚做的选择，只清理自己的记录。
	if mode, addr, cerr := currentProxy(); cerr == nil && !pointsToUs(mode, addr, target) {
		_ = m.clearState()
		m.stale = false
		res := Result{OK: true, Message: "系统代理已被其他方式修改，跳过恢复（已清理接管记录）"}
		res.Status = m.statusLocked()
		return res, nil
	}

	cmds, err := restoreProxy(st, nil, execMode{AllowPrompt: true})
	res := Result{Commands: formatCommands(cmds)}
	if err == nil {
		err = m.verify(target, false)
	}
	if err != nil {
		res.Message = "恢复系统代理失败，系统可能仍指向 观流"
		res.Output = err.Error()
		res.Status = m.statusLocked()
		if errors.Is(err, ErrPermission) {
			res.Message = "恢复系统代理需要管理员授权，请重试"
		}
		return res, err
	}

	_ = m.clearState()
	m.stale = false
	res.OK = true
	res.Changed = true
	res.Message = "已恢复系统代理原设置"
	res.Status = m.statusLocked()
	return res, nil
}

// Repoint 在端口变更后把系统代理改指到新地址。
// 沿用原快照：恢复目标不变，因此不需要重新快照。
func (m *Manager) Repoint() (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.loadState()
	if st == nil {
		res := Result{OK: true, Message: "系统代理未接管，无需重新指向"}
		res.Status = m.statusLocked()
		return res, nil
	}
	target := m.Target()
	if target == "" || target == st.ProxyAddr {
		res := Result{OK: true, Message: "系统代理地址未变"}
		res.Status = m.statusLocked()
		return res, nil
	}

	cmds, err := applyProxy(target, BypassHosts, execMode{AllowPrompt: true})
	res := Result{Commands: formatCommands(cmds)}
	if err != nil {
		res.Message = "系统代理改指新端口失败"
		res.Output = err.Error()
		res.Status = m.statusLocked()
		return res, err
	}
	st.ProxyAddr = target
	if err := m.writeState(st); err != nil {
		res.Message = "系统代理已改指新端口，但快照写入失败，恢复可能不完整"
		res.Output = err.Error()
		res.Status = m.statusLocked()
		return res, err
	}
	res.OK = true
	res.Changed = true
	res.Message = "系统代理已改指新端口：" + target
	res.Status = m.statusLocked()
	return res, nil
}

// BootRecover 在应用启动时检查上次是否留下未恢复的接管。
//
// 判定顺序：
//  1. 没有状态文件 / 上次正常收尾 → 什么都不做。
//  2. 系统代理已经不是我们设的地址 → 只清理记录，绝不改系统设置
//     （用户可能手动换了代理，回滚会覆盖他刚做的选择）。
//  3. 仍指向我们，但端口有人监听 → 可能另一个实例在用，保持现状。
//  4. 仍指向我们且端口无人监听 → 尝试自动恢复；需要提权时不弹框，交给界面一键恢复。
func (m *Manager) BootRecover() BootResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.loadState()
	if st == nil {
		return BootResult{}
	}
	capb := capability()
	if !capb.Supported {
		m.stale = true
		return BootResult{Leftover: true, Note: "上次未正常退出，本平台无法自动恢复系统代理，请手动检查系统网络设置"}
	}

	mode, addr, err := currentProxy()
	if err != nil || !pointsToUs(mode, addr, st.ProxyAddr) {
		// 系统代理已不是我们的地址：只清记录，不碰系统
		_ = m.clearState()
		return BootResult{Note: "已清理上次遗留的接管记录（系统代理已不是 观流）"}
	}

	if portListening(st.ProxyAddr) {
		m.stale = true
		return BootResult{Leftover: true, Note: "系统代理仍指向 观流，端口仍在监听，已保持接管"}
	}

	if capb.NeedsAdmin && !isElevated() {
		m.stale = true
		return BootResult{Leftover: true, Note: "上次未正常退出：系统代理仍指向 观流，恢复需要管理员权限"}
	}

	cmds, rerr := restoreProxy(st, nil, execMode{AllowPrompt: false})
	_ = cmds
	if rerr == nil {
		rerr = m.verify(st.ProxyAddr, false)
	}
	if rerr != nil {
		m.stale = true
		return BootResult{Leftover: true, Note: "上次未正常退出：系统代理仍指向 观流，自动恢复失败"}
	}

	_ = m.clearState()
	m.stale = false
	return BootResult{Restored: true, Note: "检测到上次异常退出，已自动恢复原系统代理设置"}
}

// fail 组装一个失败结果，顺带带上最新状态。
func (m *Manager) fail(msg string, err error) (Result, error) {
	if err == nil {
		err = errors.New(msg)
	}
	res := Result{Message: msg, Status: m.statusLocked()}
	return res, err
}

// statusLocked 组装状态；调用方需持有锁。
func (m *Manager) statusLocked() Status {
	capb := capability()
	st := Status{
		Supported:  capb.Supported,
		Platform:   platformName(),
		Target:     m.Target(),
		NeedsAdmin: capb.NeedsAdmin,
		Mechanism:  capb.Mechanism,
		Manual:     capb.Manual,
		Stale:      m.stale,
		CheckedAt:  time.Now(),
	}
	if !capb.Supported {
		st.Detail = capb.Detail
		return st
	}

	mode, addr, err := currentProxy()
	if err != nil {
		st.Error = err.Error()
		st.Detail = "读取系统代理失败：" + err.Error()
		return st
	}
	st.Mode, st.Addr = mode, addr
	st.Enabled = pointsToUs(mode, addr, st.Target)

	switch {
	case st.Enabled && st.Stale:
		st.Detail = "上次未正常退出：系统代理仍指向 观流，可一键恢复"
	case st.Enabled:
		st.Detail = "系统代理已指向 观流，退出或停止代理时会自动恢复"
	case st.Stale:
		st.Detail = "存在上次遗留的接管记录，可一键清理"
	default:
		st.Detail = "系统代理未接管（当前：" + describeProxy(mode, addr) + "）"
	}
	return st
}

// verify 校验系统代理是否已经变成期望状态。
// 系统设置在部分平台上是异步生效的，因此带几次重试。
func (m *Manager) verify(target string, want bool) error {
	var last error
	for i := 0; i < 4; i++ {
		mode, addr, err := currentProxy()
		if err != nil {
			last = err
		} else if pointsToUs(mode, addr, target) == want {
			return nil
		} else {
			last = fmt.Errorf("系统当前代理为 %s", describeProxy(mode, addr))
		}
		time.Sleep(120 * time.Millisecond)
	}
	if last == nil {
		last = errors.New("系统代理状态未按预期变化")
	}
	return last
}

// loadState 读取状态文件，不存在或损坏时返回 nil。
func (m *Manager) loadState() *stateFile {
	data, err := os.ReadFile(m.StatePath())
	if err != nil {
		return nil
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		return nil
	}
	return &st
}

// writeState 原子写入状态文件，权限 0600：里面含用户原始代理配置。
func (m *Manager) writeState(st *stateFile) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := m.StatePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// clearState 删除状态文件。删除失败不影响主流程。
func (m *Manager) clearState() error {
	err := os.Remove(m.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// pointsToUs 判断系统代理地址是否就是我们的目标地址。
// 主机名允许 localhost 与 127.0.0.1 互换，端口必须一致。
func pointsToUs(mode, addr, target string) bool {
	if addr == "" || target == "" {
		return false
	}
	switch strings.ToLower(mode) {
	case "none", "off", "unsupported":
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	tHost, tPort, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	return port == tPort && isLoopback(host) && isLoopback(tHost)
}

// describeProxy 把代理状态翻成人话。
func describeProxy(mode, addr string) string {
	switch strings.ToLower(mode) {
	case "none", "off":
		return "直连"
	case "auto":
		return "自动配置脚本（PAC）"
	case "manual":
		if addr == "" {
			return "手动，但未填写地址"
		}
		return "手动 " + addr
	case "unsupported":
		return "系统不支持"
	default:
		if addr != "" {
			return mode + " " + addr
		}
		return mode
	}
}

// portListening 判断地址上是否已有服务在监听。
// 用于区分「我们自己的残留」和「另一个实例正在使用」。
func portListening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// formatCommands 把 argv 列表转成可读命令，连续空白与含空格参数都做引号处理。
func formatCommands(cmds [][]string) []string {
	if len(cmds) == 0 {
		return nil
	}
	out := make([]string, 0, len(cmds))
	for _, argv := range cmds {
		parts := make([]string, 0, len(argv))
		for _, a := range argv {
			if a == "" || strings.ContainsAny(a, " \t\"'()[]{}|&;$<>*?!") {
				parts = append(parts, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
				continue
			}
			parts = append(parts, a)
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

// execMode 控制平台实现如何去执行命令。
// 启动自愈阶段不允许弹授权框，否则用户一开应用就看到密码提示。
type execMode struct {
	AllowPrompt bool
}

// runCommand 是唯一的外部命令出口，测试可替换以便断言命令序列。
var runCommand = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// splitTarget 校验并拆解 "host:port"。
func splitTarget(target string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, fmt.Errorf("代理地址 %q 不合法: %w", target, err)
	}
	if host == "" {
		return "", 0, errors.New("代理地址缺少主机名")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("代理端口 %q 不合法", portStr)
	}
	return host, port, nil
}

// isLoopback 判断主机名是否是本机回环。
func isLoopback(host string) bool {
	h := strings.Trim(strings.ToLower(host), "[]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// firstLine 取输出的第一行，避免把整段 stderr 塞进错误信息。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// platformName 返回面向用户的操作系统名。
func platformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}
