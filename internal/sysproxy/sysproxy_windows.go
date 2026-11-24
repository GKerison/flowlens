//go:build windows

package sysproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// inetSettingsKey 是 WinINet 的每用户代理配置，位于 HKCU，因此不需要管理员权限。
	inetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

	// winOverride 是接管时写入的绕过列表。
	// <local> 让不含点的主机名直连；私网段直连可避免内网服务被代理后打不开。
	winOverride = `<local>;localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*`
)

// winValue 记录一个注册表值的原始形态，恢复时按原类型写回。
type winValue struct {
	Exists bool   `json:"exists"`
	Type   uint32 `json:"type,omitempty"`
	Dword  uint32 `json:"dword,omitempty"`
	String string `json:"string,omitempty"`
	Binary []byte `json:"binary,omitempty"`
}

// winSnapshot 是接管前的注册表现场。
type winSnapshot struct {
	ProxyEnable   winValue `json:"proxyEnable"`
	ProxyServer   winValue `json:"proxyServer"`
	ProxyOverride winValue `json:"proxyOverride"`
	AutoConfigURL winValue `json:"autoConfigUrl"`
	AutoDetect    winValue `json:"autoDetect"`
}

// capability 汇报接管能力。
// Windows 的代理设置属于当前用户，写 HKCU 不触发 UAC。
func capability() Capability {
	return Capability{
		Supported:  true,
		Mechanism:  "WinINet 注册表",
		NeedsAdmin: false,
		Detail:     "修改当前用户的 Internet 选项代理设置，并广播给正在运行的程序",
		Manual: []string{
			"打开「设置」→「网络和 Internet」→「代理」",
			"在「手动设置代理」中打开「使用代理服务器」",
			"地址填 127.0.0.1，端口填 观流 的代理端口，点击保存",
			"在「请勿对以下列条目开头的地址使用代理服务器」中保留 localhost;127.*",
		},
	}
}

// isElevated 表示是否需要提权；Windows 下接管不需要。
func isElevated() bool { return true }

// readSnapshot 读取当前用户的代理配置。
func readSnapshot() (json.RawMessage, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return nil, fmt.Errorf("打开 Internet 设置失败: %w", err)
	}
	defer k.Close()

	snap := winSnapshot{
		ProxyEnable:   readValue(k, "ProxyEnable"),
		ProxyServer:   readValue(k, "ProxyServer"),
		ProxyOverride: readValue(k, "ProxyOverride"),
		AutoConfigURL: readValue(k, "AutoConfigURL"),
		AutoDetect:    readValue(k, "AutoDetect"),
	}
	return json.Marshal(snap)
}

// applyProxy 写入代理配置并通知系统。
func applyProxy(target string, _ []string, _ execMode) ([][]string, error) {
	if _, _, err := splitTarget(target); err != nil {
		return nil, err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("打开 Internet 设置失败: %w", err)
	}
	defer k.Close()

	cmds := winApplyCommands(target)

	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return cmds, fmt.Errorf("启用代理失败: %w", err)
	}
	if err := k.SetStringValue("ProxyServer", target); err != nil {
		return cmds, fmt.Errorf("写入代理地址失败: %w", err)
	}
	if err := k.SetStringValue("ProxyOverride", winOverride); err != nil {
		return cmds, fmt.Errorf("写入绕过列表失败: %w", err)
	}
	// PAC 与 WPAD 的优先级高于手动代理，留着会让「设置成功但不生效」
	if err := k.DeleteValue("AutoConfigURL"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return cmds, fmt.Errorf("关闭 PAC 失败: %w", err)
	}
	if err := k.SetDWordValue("AutoDetect", 0); err != nil {
		return cmds, fmt.Errorf("关闭自动检测失败: %w", err)
	}

	notifyChange()
	return cmds, nil
}

// restoreProxy 按快照写回；没有快照时只关闭代理开关。
func restoreProxy(st *stateFile, _ []string, _ execMode) ([][]string, error) {
	if st == nil || len(st.Snapshot) == 0 {
		return disableProxy(execMode{})
	}
	var snap winSnapshot
	if err := json.Unmarshal(st.Snapshot, &snap); err != nil {
		return nil, fmt.Errorf("接管快照损坏，无法自动恢复: %w", err)
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("打开 Internet 设置失败: %w", err)
	}
	defer k.Close()

	var cmds [][]string
	for _, item := range []struct {
		name string
		val  winValue
	}{
		{"ProxyEnable", snap.ProxyEnable},
		{"ProxyServer", snap.ProxyServer},
		{"ProxyOverride", snap.ProxyOverride},
		{"AutoConfigURL", snap.AutoConfigURL},
		{"AutoDetect", snap.AutoDetect},
	} {
		if err := writeValue(k, item.name, item.val); err != nil {
			return append(cmds, regCommand(item.name, item.val)), fmt.Errorf("恢复 %s 失败: %w", item.name, err)
		}
		cmds = append(cmds, regCommand(item.name, item.val))
	}

	notifyChange()
	return cmds, nil
}

// disableProxy 关闭代理开关（无快照时的兜底）。
func disableProxy(_ execMode) ([][]string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("打开 Internet 设置失败: %w", err)
	}
	defer k.Close()
	if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
		return nil, fmt.Errorf("关闭代理失败: %w", err)
	}
	notifyChange()
	return [][]string{{"reg", "add", `HKCU\` + inetSettingsKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f"}}, nil
}

// currentProxy 读取当前生效的代理。
func currentProxy() (string, string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return "", "", fmt.Errorf("打开 Internet 设置失败: %w", err)
	}
	defer k.Close()

	enabled := readValue(k, "ProxyEnable")
	if enabled.Exists && enabled.Dword == 0 {
		return "none", "", nil
	}
	addr := parseProxyServer(readValue(k, "ProxyServer").String)
	if enabled.Exists && enabled.Dword == 1 && addr != "" {
		return "manual", addr, nil
	}
	if pac := readValue(k, "AutoConfigURL"); pac.Exists && strings.TrimSpace(pac.String) != "" {
		return "auto", pac.String, nil
	}
	return "none", "", nil
}

// readValue 读取单个注册表值，不存在时返回零值。
// 先取类型再取值：同一个键下可能混用 REG_DWORD / REG_SZ，不能猜。
func readValue(k registry.Key, name string) winValue {
	_, typ, err := k.GetValue(name, nil)
	if err != nil {
		return winValue{}
	}
	switch typ {
	case registry.DWORD, registry.QWORD:
		v, _, err := k.GetIntegerValue(name)
		if err != nil {
			return winValue{}
		}
		return winValue{Exists: true, Type: typ, Dword: uint32(v)}
	case registry.SZ, registry.EXPAND_SZ:
		v, _, err := k.GetStringValue(name)
		if err != nil {
			return winValue{}
		}
		return winValue{Exists: true, Type: typ, String: v}
	default:
		v, _, err := k.GetBinaryValue(name)
		if err != nil {
			return winValue{Exists: true, Type: typ}
		}
		return winValue{Exists: true, Type: typ, Binary: v}
	}
}

// writeValue 按原类型写回；原本不存在的值要删掉，而不是写成空值。
func writeValue(k registry.Key, name string, v winValue) error {
	if !v.Exists {
		if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	switch v.Type {
	case registry.DWORD, registry.QWORD:
		return k.SetDWordValue(name, v.Dword)
	case registry.EXPAND_SZ:
		return k.SetExpandStringValue(name, v.String)
	case registry.BINARY:
		return k.SetBinaryValue(name, v.Binary)
	default:
		return k.SetStringValue(name, v.String)
	}
}

// winApplyCommands 生成等价的 reg 命令，供界面展示与实际写入对照。
func winApplyCommands(target string) [][]string {
	key := `HKCU\` + inetSettingsKey
	return [][]string{
		{"reg", "add", key, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f"},
		{"reg", "add", key, "/v", "ProxyServer", "/t", "REG_SZ", "/d", target, "/f"},
		{"reg", "add", key, "/v", "ProxyOverride", "/t", "REG_SZ", "/d", winOverride, "/f"},
		{"reg", "delete", key, "/v", "AutoConfigURL", "/f"},
	}
}

// regCommand 为一个快照值生成等价的 reg 命令（仅用于展示）。
func regCommand(name string, v winValue) []string {
	key := `HKCU\` + inetSettingsKey
	if !v.Exists {
		return []string{"reg", "delete", key, "/v", name, "/f"}
	}
	switch v.Type {
	case registry.DWORD, registry.QWORD:
		return []string{"reg", "add", key, "/v", name, "/t", "REG_DWORD", "/d", strconv.Itoa(int(v.Dword)), "/f"}
	default:
		return []string{"reg", "add", key, "/v", name, "/t", "REG_SZ", "/d", v.String, "/f"}
	}
}

// parseProxyServer 兼容 "host:port" 与 "http=host:port;https=host:port" 两种写法。
func parseProxyServer(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "=") {
		return s
	}
	fallback := ""
	for _, part := range strings.Split(s, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		proto := strings.ToLower(strings.TrimSpace(kv[0]))
		addr := strings.TrimSpace(kv[1])
		if proto == "https" {
			return addr
		}
		if fallback == "" {
			fallback = addr
		}
	}
	return fallback
}

// notifyChange 通知系统代理设置已变。
// 只写注册表不够：已在运行的浏览器/客户端会继续用旧配置，必须广播。
func notifyChange() {
	if dll := windows.NewLazySystemDLL("wininet.dll"); dll.Load() == nil {
		proc := dll.NewProc("InternetSetOptionW")
		const (
			optionSettingsChanged = 39
			optionRefresh         = 37
		)
		_, _, _ = proc.Call(0, optionSettingsChanged, 0, 0)
		_, _, _ = proc.Call(0, optionRefresh, 0, 0)
	}

	if dll := windows.NewLazySystemDLL("user32.dll"); dll.Load() == nil {
		proc := dll.NewProc("SendMessageTimeoutW")
		p, err := windows.UTF16PtrFromString(inetSettingsKey)
		if err != nil {
			return
		}
		const (
			hwndBroadcast   = 0xffff
			wmSettingChange = 0x001A
			smtoAbortIfHung = 0x0002
		)
		var result uintptr
		_, _, _ = proc.Call(hwndBroadcast, wmSettingChange, 0,
			uintptr(unsafe.Pointer(p)), smtoAbortIfHung, 1000, uintptr(unsafe.Pointer(&result)))
	}
}
