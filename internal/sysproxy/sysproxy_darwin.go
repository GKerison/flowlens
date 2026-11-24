//go:build darwin

package sysproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// networksetup 是 macOS 修改网络配置的官方命令行工具。
// 写操作会改 /Library/Preferences/SystemConfiguration，普通用户需要授权。
const networksetup = "/usr/sbin/networksetup"

// scutil 读取「当前生效」的代理配置（只读，无需授权）。
const scutil = "/usr/sbin/scutil"

// darwinService 是一个网络服务（Wi-Fi、以太网…）的代理原始配置。
// 必须逐服务记录：用户可能同时开着 Wi-Fi 和 USB 网卡，只恢复其中一个会留下残留。
type darwinService struct {
	Name          string   `json:"name"`
	WebEnabled    bool     `json:"webEnabled"`
	WebServer     string   `json:"webServer,omitempty"`
	WebPort       int      `json:"webPort,omitempty"`
	SecureEnabled bool     `json:"secureEnabled"`
	SecureServer  string   `json:"secureServer,omitempty"`
	SecurePort    int      `json:"securePort,omitempty"`
	AutoEnabled   bool     `json:"autoEnabled"`
	AutoURL       string   `json:"autoUrl,omitempty"`
	Bypass        []string `json:"bypass,omitempty"`
}

type darwinSnapshot struct {
	Services []darwinService `json:"services"`
}

// capability 汇报接管能力。
func capability() Capability {
	c := Capability{
		Supported:  true,
		Mechanism:  "networksetup",
		NeedsAdmin: os.Geteuid() != 0,
		Detail:     "通过 networksetup 修改当前所有启用中网络服务的代理设置",
		Manual: []string{
			"打开「系统设置」→「网络」→ 选中当前使用的服务 →「详细信息…」",
			"切到「代理」页，勾选「网页代理(HTTP)」与「安全网页代理(HTTPS)」",
			"服务器填 127.0.0.1，端口填 观流 的代理端口",
			"在「忽略这些主机与域的代理设置」中加入 localhost, 127.0.0.1",
		},
	}
	if _, err := exec.LookPath("networksetup"); err != nil {
		c.Supported = false
		c.Detail = "未找到 networksetup，无法自动接管系统代理"
	}
	return c
}

// isElevated 表示当前进程是否已经是 root。
// 做成变量是为了让集成测试能注入「已提权」，从而直接走执行分支而不弹系统授权框。
var isElevated = func() bool { return os.Geteuid() == 0 }

// readSnapshot 读取所有启用中网络服务的代理配置。
func readSnapshot() (json.RawMessage, error) {
	services, err := darwinServices()
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, errors.New("没有找到可配置的网络服务")
	}
	snap := darwinSnapshot{}
	for _, name := range services {
		svc, err := readService(name)
		if err != nil {
			return nil, fmt.Errorf("读取网络服务「%s」失败: %w", name, err)
		}
		snap.Services = append(snap.Services, svc)
	}
	return json.Marshal(snap)
}

// darwinServices 返回启用中的网络服务名。
func darwinServices() ([]string, error) {
	out, err := runCommand(networksetup, "-listallnetworkservices")
	if err != nil {
		return nil, wrapNetworkError(out, err)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// 首行是说明文字；带 * 前缀的服务已被禁用，改它没有意义
		if line == "" || strings.HasPrefix(line, "An asterisk") || strings.HasPrefix(line, "*") {
			continue
		}
		names = append(names, line)
	}
	return names, nil
}

// readService 读取单个服务的四项配置。
func readService(name string) (darwinService, error) {
	svc := darwinService{Name: name}

	out, err := runCommand(networksetup, "-getwebproxy", name)
	if err != nil {
		return svc, wrapNetworkError(out, err)
	}
	kv := parseKV(out)
	svc.WebEnabled = parseBool(kv["Enabled"])
	svc.WebServer = kv["Server"]
	svc.WebPort = parseInt(kv["Port"])

	out, err = runCommand(networksetup, "-getsecurewebproxy", name)
	if err != nil {
		return svc, wrapNetworkError(out, err)
	}
	kv = parseKV(out)
	svc.SecureEnabled = parseBool(kv["Enabled"])
	svc.SecureServer = kv["Server"]
	svc.SecurePort = parseInt(kv["Port"])

	out, err = runCommand(networksetup, "-getautoproxyurl", name)
	if err != nil {
		return svc, wrapNetworkError(out, err)
	}
	kv = parseKV(out)
	svc.AutoEnabled = parseBool(kv["Enabled"])
	if u := kv["URL"]; u != "" && u != "(null)" {
		svc.AutoURL = u
	}

	// 绕过列表读失败不影响接管（最多是恢复时少还原一项），不当作致命错误
	if out, err := runCommand(networksetup, "-getproxybypassdomains", name); err == nil {
		svc.Bypass = parseBypass(out)
	}
	return svc, nil
}

// applyProxy 把系统代理指向 target。
func applyProxy(target string, bypass []string, mode execMode) ([][]string, error) {
	host, port, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	services, err := darwinServices()
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, errors.New("没有找到可配置的网络服务")
	}

	var cmds [][]string
	for _, svc := range services {
		cmds = append(cmds,
			[]string{networksetup, "-setwebproxy", svc, host, strconv.Itoa(port)},
			[]string{networksetup, "-setsecurewebproxy", svc, host, strconv.Itoa(port)},
			// PAC 优先级高于手动代理，不关掉会让我们的设置"看起来生效、实际没用"
			[]string{networksetup, "-setautoproxystate", svc, "off"},
			append([]string{networksetup, "-setproxybypassdomains", svc}, bypass...),
		)
	}
	return cmds, runCommands(cmds, mode)
}

// restoreProxy 按快照还原。
// st 为空或没有快照时退化为「关闭代理」，适用于接管前系统本来就指向我们的情况。
func restoreProxy(st *stateFile, _ []string, mode execMode) ([][]string, error) {
	if st == nil || len(st.Snapshot) == 0 {
		return disableProxy(mode)
	}
	var snap darwinSnapshot
	if err := json.Unmarshal(st.Snapshot, &snap); err != nil {
		return nil, fmt.Errorf("接管快照损坏，无法自动恢复: %w", err)
	}

	var cmds [][]string
	for _, svc := range snap.Services {
		// PAC
		if svc.AutoEnabled && svc.AutoURL != "" {
			cmds = append(cmds,
				[]string{networksetup, "-setautoproxyurl", svc.Name, svc.AutoURL},
				[]string{networksetup, "-setautoproxystate", svc.Name, "on"},
			)
		} else {
			cmds = append(cmds, []string{networksetup, "-setautoproxystate", svc.Name, "off"})
		}
		// HTTP
		if svc.WebEnabled && svc.WebServer != "" {
			cmds = append(cmds, []string{networksetup, "-setwebproxy", svc.Name, svc.WebServer, strconv.Itoa(svc.WebPort)})
		} else {
			cmds = append(cmds, []string{networksetup, "-setwebproxystate", svc.Name, "off"})
		}
		// HTTPS
		if svc.SecureEnabled && svc.SecureServer != "" {
			cmds = append(cmds, []string{networksetup, "-setsecurewebproxy", svc.Name, svc.SecureServer, strconv.Itoa(svc.SecurePort)})
		} else {
			cmds = append(cmds, []string{networksetup, "-setsecurewebproxystate", svc.Name, "off"})
		}
		// 绕过列表：空列表必须显式写 Empty，否则旧的绕过项会残留
		if len(svc.Bypass) == 0 {
			cmds = append(cmds, []string{networksetup, "-setproxybypassdomains", svc.Name, "Empty"})
		} else {
			cmds = append(cmds, append([]string{networksetup, "-setproxybypassdomains", svc.Name}, svc.Bypass...))
		}
	}
	return cmds, runCommands(cmds, mode)
}

// disableProxy 关闭所有网络服务的代理开关（无快照时的兜底）。
func disableProxy(mode execMode) ([][]string, error) {
	services, err := darwinServices()
	if err != nil {
		return nil, err
	}
	var cmds [][]string
	for _, svc := range services {
		cmds = append(cmds,
			[]string{networksetup, "-setwebproxystate", svc, "off"},
			[]string{networksetup, "-setsecurewebproxystate", svc, "off"},
			[]string{networksetup, "-setautoproxystate", svc, "off"},
		)
	}
	return cmds, runCommands(cmds, mode)
}

// currentProxy 读取当前生效的代理。
// 用 scutil 而不是逐个服务查询：它给出的是系统真正在用的那份配置。
func currentProxy() (string, string, error) {
	out, err := runCommand(scutil, "--proxy")
	if err != nil {
		return "", "", fmt.Errorf("读取系统代理状态失败: %w", err)
	}
	kv := parseKV(out)

	if parseBool(kv["HTTPSEnable"]) {
		if addr := joinHostPort(kv["HTTPSProxy"], kv["HTTPSPort"]); addr != "" {
			return "manual", addr, nil
		}
	}
	if parseBool(kv["HTTPEnable"]) {
		if addr := joinHostPort(kv["HTTPProxy"], kv["HTTPPort"]); addr != "" {
			return "manual", addr, nil
		}
	}
	if parseBool(kv["ProxyAutoConfigEnable"]) {
		return "auto", kv["ProxyAutoConfigURLString"], nil
	}
	if parseBool(kv["SOCKSEnable"]) {
		if addr := joinHostPort(kv["SOCKSProxy"], kv["SOCKSPort"]); addr != "" {
			return "manual", addr, nil
		}
	}
	return "none", "", nil
}

// runCommands 执行命令列表。
//
// 提权策略：非 root 时用一个 osascript 把所有命令包进同一次授权，
// 用户只看到一个系统密码框，而不是每个服务弹一次。
// mode.AllowPrompt=false 时（启动自愈阶段）不弹框，直接返回 ErrPermission。
func runCommands(cmds [][]string, mode execMode) error {
	if len(cmds) == 0 {
		return nil
	}
	if isElevated() {
		for _, argv := range cmds {
			out, err := runCommand(argv[0], argv[1:]...)
			if err != nil {
				return wrapNetworkError(out, err)
			}
		}
		return nil
	}
	if !mode.AllowPrompt {
		return fmt.Errorf("%w：修改系统网络设置需要授权", ErrPermission)
	}

	var script strings.Builder
	for i, argv := range cmds {
		if i > 0 {
			script.WriteString(" && ")
		}
		for j, a := range argv {
			if j > 0 {
				script.WriteByte(' ')
			}
			script.WriteString(shQuote(a))
		}
	}

	line := fmt.Sprintf("do shell script %s with prompt %s with administrator privileges",
		asQuote(script.String()), asQuote("观流 需要管理员权限修改系统代理设置"))
	if out, err := runCommand("osascript", "-e", line); err != nil {
		return wrapNetworkError(out, err)
	}
	return nil
}

// parseKV 解析 networksetup 与 scutil 的 "Key: Value" / "Key : Value" 输出。
// 按第一个冒号切分，避免 IPv6 地址与 URL 里的冒号被误切。
func parseKV(out string) map[string]string {
	kv := make(map[string]string, 8)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		kv[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
	}
	return kv
}

// parseBypass 解析 -getproxybypassdomains 的输出（一行一个域名）。
func parseBypass(out string) []string {
	var list []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "There aren't any") {
			continue
		}
		list = append(list, line)
	}
	return list
}

// parseBool 识别 networksetup / scutil 的布尔表示。
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "yes", "on", "true":
		return true
	}
	return false
}

// parseInt 宽松解析整数，失败返回 0。
func parseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// joinHostPort 把 host 与 port 拼成 host:port，缺一返回空。
func joinHostPort(host, port string) string {
	host, port = strings.TrimSpace(host), strings.TrimSpace(port)
	if host == "" || port == "" || port == "0" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// wrapNetworkError 把系统工具的错误输出翻成用户能理解的错误，并识别「权限不足」。
func wrapNetworkError(out string, err error) error {
	msg := strings.TrimSpace(out)
	if msg == "" {
		msg = err.Error()
	}
	low := strings.ToLower(msg)
	for _, kw := range []string{"not permitted", "permission denied", "must be root", "authorization", "user canceled", "(-128)"} {
		if strings.Contains(low, kw) {
			return fmt.Errorf("%w：%s", ErrPermission, firstLine(msg))
		}
	}
	return errors.New(firstLine(msg))
}

// shQuote 用单引号包裹字符串，供 shell 拼接。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// asQuote 把字符串包成 AppleScript 的双引号字面量。
func asQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
