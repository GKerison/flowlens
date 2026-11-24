//go:build linux

package trust

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// anchor 描述一个系统信任锚点目录及其刷新命令，按发行版差异排列。
// 不做发行版识别（/etc/os-release 判断常出错），而是探测目录是否存在，命中即用。
type anchor struct {
	dir      string   // 证书存放目录
	fileName string   // 落地文件名
	refresh  []string // 刷新系统证书库的命令
}

var anchorCandidates = []anchor{
	{dir: "/usr/local/share/ca-certificates", fileName: "flowlens-ca.crt", refresh: []string{"update-ca-certificates"}},           // Debian/Ubuntu
	{dir: "/etc/pki/trust/anchors", fileName: "flowlens-ca.crt", refresh: []string{"update-ca-trust", "extract"}},                 // Fedora/RHEL/openSUSE
	{dir: "/etc/ca-certificates/trust-source/anchors", fileName: "flowlens-ca.crt", refresh: []string{"trust", "extract-compat"}}, // Arch
	{dir: "/etc/ssl/certs", fileName: "flowlens-ca.crt", refresh: []string{"update-ca-certificates"}},                             // 兜底
}

// pickAnchor 返回当前系统可用的锚点目录；都不存在时退回 Debian 约定。
func pickAnchor() anchor {
	for _, a := range anchorCandidates {
		if st, err := os.Stat(a.dir); err == nil && st.IsDir() {
			return a
		}
	}
	return anchorCandidates[0]
}

// currentStore 描述默认安装目标。
// Linux 没有"用户级系统信任库"的统一概念（各应用各用 NSS 库），只能写系统锚点。
func currentStore() Store {
	a := pickAnchor()
	return Store{
		Name:     "系统信任锚点 " + a.dir,
		Scope:    "system",
		Requires: "需要管理员权限（通过 pkexec 弹窗授权）",
	}
}

// canElevate 判断是否有可用的提权工具。
// 没有 pkexec 时只能提示用户手动执行，不擅自调用 sudo（无 TTY 会挂死）。
func canElevate() bool {
	_, err := exec.LookPath("pkexec")
	return err == nil
}

// installCommand 返回可预览的命令。
func installCommand(m *Manager, elevated bool) []string {
	a := pickAnchor()
	// Linux 无论是否提权都写系统目录，elevated 参数在这里只影响提示文案
	return append([]string{"pkexec"}, "sh", "-c",
		fmt.Sprintf("install -m 0644 %s %s && %s",
			shQuote(m.certPath), shQuote(filepath.Join(a.dir, a.fileName)), strings.Join(a.refresh, " ")))
}

// detect 检查系统锚点里是否存在与本证书一致的文件。
func detect(m *Manager) (trusted, elevated bool, detail string, cmd []string) {
	cmd = installCommand(m, false)
	a := pickAnchor()
	path := filepath.Join(a.dir, a.fileName)

	data, err := os.ReadFile(path)
	if err == nil {
		if sameCert(data, m.cert) {
			return true, true, "已安装到系统信任锚点 " + path, cmd
		}
		return false, false, "锚点目录中存在同名但内容不同的证书，建议先移除再安装", cmd
	}
	if os.IsNotExist(err) {
		return false, false, "尚未安装到系统信任库，HTTPS 会被浏览器拦截", cmd
	}
	return false, false, "无法读取系统信任锚点：" + err.Error(), cmd
}

// install 把证书安装到系统锚点并刷新证书库。
// pkexec 会弹出图形化授权框，与 macOS 的 osascript 体验一致。
func install(m *Manager, elevated bool) (Result, error) {
	res := Result{Command: installCommand(m, elevated)}
	if !canElevate() {
		return res, fmt.Errorf("未找到 pkexec，请手动执行：%s", strings.Join(res.Command, " "))
	}

	a := pickAnchor()
	inner := fmt.Sprintf("install -m 0644 %s %s && %s",
		shQuote(m.certPath), shQuote(filepath.Join(a.dir, a.fileName)), strings.Join(a.refresh, " "))
	out, err := runCmd("pkexec", "sh", "-c", inner)
	if err != nil {
		res.Message = "安装失败"
		res.Output = out
		return res, fmt.Errorf("安装到系统信任锚点失败: %w", err)
	}

	trusted, _, detail, _ := detect(m)
	res.OK = trusted
	res.Message = detail
	if !trusted {
		return res, errNotApplied
	}
	return res, nil
}

// uninstall 删除锚点文件并刷新证书库。
func uninstall(m *Manager) (Result, error) {
	a := pickAnchor()
	path := filepath.Join(a.dir, a.fileName)
	res := Result{Command: []string{"pkexec", "sh", "-c", "rm -f " + shQuote(path) + " && " + strings.Join(a.refresh, " ")}}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		res.OK = true
		res.Message = "系统信任库中未找到该证书，无需移除"
		return res, nil
	}
	if !canElevate() {
		return res, fmt.Errorf("未找到 pkexec，请手动执行：%s", strings.Join(res.Command, " "))
	}

	inner := fmt.Sprintf("rm -f %s && %s", shQuote(path), strings.Join(a.refresh, " "))
	out, err := runCmd("pkexec", "sh", "-c", inner)
	if err != nil {
		res.Message = "移除失败"
		res.Output = out
		return res, fmt.Errorf("从系统信任锚点移除失败: %w", err)
	}
	res.OK = true
	res.Message = "已从系统信任库移除（部分应用重启后生效）"
	return res, nil
}

// sameCert 判断 PEM 内容是否就是目标证书。
func sameCert(data []byte, target *x509.Certificate) bool {
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" && bytes.Equal(block.Bytes, target.Raw) {
			return true
		}
	}
}

// shQuote 用单引号包裹字符串，避免路径含空格时被 shell 拆词。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
