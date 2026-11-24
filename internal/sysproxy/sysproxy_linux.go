//go:build linux

package sysproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// gsettingsBin 是 GNOME 桌面的配置读写工具。
const gsettingsBin = "gsettings"

// gsValue 记录一个 gsettings 键的原始 GVariant 文本，恢复时原样写回。
type gsValue struct {
	Schema string `json:"schema"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

type linuxSnapshot struct {
	Values []gsValue `json:"values"`
}

// gsKeys 是接管涉及的全部配置键。
// 与代理相关的键分散在三个 schema 里，必须逐个快照，否则恢复时会丢配置。
var gsKeys = []struct{ schema, key string }{
	{"org.gnome.system.proxy", "mode"},
	{"org.gnome.system.proxy", "ignore-hosts"},
	{"org.gnome.system.proxy.http", "host"},
	{"org.gnome.system.proxy.http", "port"},
	{"org.gnome.system.proxy.https", "host"},
	{"org.gnome.system.proxy.https", "port"},
}

// capability 汇报接管能力：只有 GNOME 系桌面才有 gsettings 代理 schema。
func capability() Capability {
	c := Capability{
		Mechanism: "GNOME gsettings",
		Detail:    "通过 gsettings 修改 GNOME 桌面代理设置",
		Manual: []string{
			"打开「设置」→「网络」→「网络代理」",
			"选择「手动」，在 HTTP 与 HTTPS 代理中填 127.0.0.1 与 观流 的代理端口",
			"在「忽略主机」中加入 localhost, 127.0.0.1",
			"非 GNOME 桌面（KDE/XFCE）请在该桌面的网络设置里手动配置",
		},
	}
	if _, err := exec.LookPath(gsettingsBin); err != nil {
		c.Detail = "未找到 gsettings，当前桌面环境无法自动接管系统代理"
		return c
	}
	if _, err := runCommand(gsettingsBin, "get", "org.gnome.system.proxy", "mode"); err != nil {
		c.Detail = "当前桌面没有 GNOME 代理配置（schema 缺失），请手动配置"
		return c
	}
	c.Supported = true
	return c
}

// isElevated：gsettings 属于当前用户配置，不需要提权。
func isElevated() bool { return true }

// readSnapshot 读取全部相关键的原始值。
func readSnapshot() (json.RawMessage, error) {
	snap := linuxSnapshot{}
	for _, item := range gsKeys {
		out, err := runCommand(gsettingsBin, "get", item.schema, item.key)
		if err != nil {
			return nil, fmt.Errorf("读取 %s %s 失败: %w", item.schema, item.key, err)
		}
		snap.Values = append(snap.Values, gsValue{Schema: item.schema, Key: item.key, Value: strings.TrimSpace(out)})
	}
	return json.Marshal(snap)
}

// applyProxy 把 GNOME 代理切成手动并指向 target。
func applyProxy(target string, bypass []string, _ execMode) ([][]string, error) {
	host, port, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	cmds := [][]string{
		{gsettingsBin, "set", "org.gnome.system.proxy", "mode", gvString("manual")},
		{gsettingsBin, "set", "org.gnome.system.proxy.http", "host", gvString(host)},
		{gsettingsBin, "set", "org.gnome.system.proxy.http", "port", strconv.Itoa(port)},
		{gsettingsBin, "set", "org.gnome.system.proxy.https", "host", gvString(host)},
		{gsettingsBin, "set", "org.gnome.system.proxy.https", "port", strconv.Itoa(port)},
		{gsettingsBin, "set", "org.gnome.system.proxy", "ignore-hosts", gvStringArray(bypass)},
	}
	return cmds, runCommands(cmds)
}

// restoreProxy 按快照逐键还原；没有快照时只把 mode 切回 none。
func restoreProxy(st *stateFile, _ []string, mode execMode) ([][]string, error) {
	if st == nil || len(st.Snapshot) == 0 {
		return disableProxy(mode)
	}
	var snap linuxSnapshot
	if err := json.Unmarshal(st.Snapshot, &snap); err != nil {
		return nil, fmt.Errorf("接管快照损坏，无法自动恢复: %w", err)
	}
	var cmds [][]string
	for _, v := range snap.Values {
		cmds = append(cmds, []string{gsettingsBin, "set", v.Schema, v.Key, v.Value})
	}
	return cmds, runCommands(cmds)
}

// disableProxy 关闭代理。
func disableProxy(_ execMode) ([][]string, error) {
	cmds := [][]string{{gsettingsBin, "set", "org.gnome.system.proxy", "mode", gvString("none")}}
	return cmds, runCommands(cmds)
}

// currentProxy 读取当前 GNOME 代理配置。
func currentProxy() (string, string, error) {
	out, err := runCommand(gsettingsBin, "get", "org.gnome.system.proxy", "mode")
	if err != nil {
		return "", "", fmt.Errorf("读取系统代理失败: %w", err)
	}
	mode := unquoteGVariant(out)
	switch mode {
	case "manual":
		host, _ := runCommand(gsettingsBin, "get", "org.gnome.system.proxy.https", "host")
		port, _ := runCommand(gsettingsBin, "get", "org.gnome.system.proxy.https", "port")
		h, p := unquoteGVariant(host), strings.TrimSpace(port)
		if h == "" || p == "" || p == "0" {
			return "manual", "", nil
		}
		return "manual", h + ":" + p, nil
	case "auto":
		return "auto", "", nil
	case "none":
		return "none", "", nil
	default:
		return mode, "", nil
	}
}

// runCommands 顺序执行命令，遇到第一个失败就返回（Linux 无需提权）。
func runCommands(cmds [][]string) error {
	for _, argv := range cmds {
		out, err := runCommand(argv[0], argv[1:]...)
		if err != nil {
			msg := strings.TrimSpace(out)
			if msg == "" {
				msg = err.Error()
			}
			return errors.New(firstLine(msg))
		}
	}
	return nil
}

// gvString 把 Go 字符串转成 GVariant 字符串字面量（需要单引号）。
func gvString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// gvStringArray 把字符串切片转成 GVariant 的字符串数组。
func gvStringArray(items []string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, gvString(it))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// unquoteGVariant 去掉 gsettings 输出外围的单引号。
func unquoteGVariant(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@as ")
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `\'`, "'")
	}
	return s
}
