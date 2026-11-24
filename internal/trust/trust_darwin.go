//go:build darwin

package trust

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemKeychain 是 macOS 的系统钥匙串路径，写入它需要管理员权限。
const systemKeychain = "/Library/Keychains/System.keychain"

// loginKeychain 返回当前用户的登录钥匙串路径。
// 用户级信任只需写入这里，不必提权，是默认安装方式。
func loginKeychain() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db")
}

// currentStore 描述默认安装目标，供界面展示"影响范围"。
func currentStore() Store {
	return Store{
		Name:     "登录钥匙串（当前用户）",
		Scope:    "user",
		Requires: "",
	}
}

// canElevate 表示此平台支持改用系统级安装。
func canElevate() bool { return true }

// detect 检查两张钥匙串里是否存在与本证书完全一致的公钥证书。
// 只按字节比对，不信任名称——用户可能有同名但不同密钥的旧证书。
func detect(m *Manager) (trusted, elevated bool, detail string, cmd []string) {
	cmd = installCommand(m, false)

	if p := loginKeychain(); p != "" {
		if ok, err := keychainHasCert(p, m.cert); err == nil && ok {
			return true, false, "已在「登录」钥匙串中信任，仅对当前用户生效", cmd
		}
	}
	if ok, err := keychainHasCert(systemKeychain, m.cert); err == nil && ok {
		return true, true, "已在系统钥匙串中信任，对本机所有用户生效", cmd
	}
	return false, false, "尚未安装到本机信任库，HTTPS 会被浏览器拦截", cmd
}

// keychainHasCert 判断指定钥匙串是否包含目标证书。
// `security find-certificate -a -p` 输出的是多个 PEM 串接，逐个解析比对 DER。
func keychainHasCert(keychain string, target *x509.Certificate) (bool, error) {
	out, err := runCmd("security", "find-certificate", "-a", "-p", keychain)
	if err != nil {
		return false, err
	}
	rest := []byte(out)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false, nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if bytes.Equal(block.Bytes, target.Raw) {
			return true, nil
		}
	}
}

// installCommand 返回"将要执行"的命令，供界面预览。
// 提权方式显示为 sudo 形式：比展示 osascript 包装器更贴近用户心智。
func installCommand(m *Manager, elevated bool) []string {
	if elevated {
		return []string{"sudo", "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", systemKeychain, m.certPath}
	}
	return []string{"security", "add-trusted-cert", "-r", "trustRoot", "-k", loginKeychain(), m.certPath}
}

// install 把证书加入钥匙串并设置"始终信任"。
// elevated=false 写用户钥匙串（无需密码）；true 写系统钥匙串（弹管理员授权框）。
func install(m *Manager, elevated bool) (Result, error) {
	res := Result{Command: installCommand(m, elevated)}

	if elevated {
		// 系统级写入必须提权。用 osascript 触发系统原生授权弹窗，
		// 密码由系统收集，本进程既不接触也不保存。
		inner := fmt.Sprintf("security add-trusted-cert -d -r trustRoot -k %s %s",
			shQuote(systemKeychain), shQuote(m.certPath))
		script := fmt.Sprintf("do shell script %s with prompt %s with administrator privileges",
			asQuote(inner), asQuote("观流 需要管理员权限把根证书安装到系统信任库"))
		out, err := runCmd("osascript", "-e", script)
		if err != nil {
			res.Message = "系统级安装失败"
			res.Output = out
			return res, fmt.Errorf("系统级安装失败: %w", err)
		}
	} else {
		out, err := runCmd("security", "add-trusted-cert", "-r", "trustRoot",
			"-k", loginKeychain(), m.certPath)
		if err != nil {
			res.Message = "安装失败"
			res.Output = out
			return res, fmt.Errorf("安装到登录钥匙串失败: %w", err)
		}
	}

	// 以检测结果为准，不因命令返回 0 就向用户宣布成功
	trusted, _, detail, _ := detect(m)
	res.OK = trusted
	res.Message = detail
	if !trusted {
		return res, fmt.Errorf("命令已执行，但在钥匙串中未找到证书，请打开「钥匙串访问」确认")
	}
	return res, nil
}

// uninstall 从两张钥匙串中移除本证书。
// 用户级与系统级都要清理，否则"看起来卸载了、浏览器仍然信任"。
func uninstall(m *Manager) (Result, error) {
	res := Result{Command: append([]string{"security", "delete-certificate"}, "-Z", m.SHA1()+" …")}

	var removed []string
	var failures []string

	if p := loginKeychain(); p != "" {
		if ok, _ := keychainHasCert(p, m.cert); ok {
			out, err := runCmd("security", "delete-certificate", "-Z", m.SHA1(), "-t", p)
			if err != nil {
				failures = append(failures, "登录钥匙串: "+firstLine(out)+" "+err.Error())
			} else {
				removed = append(removed, "登录钥匙串")
			}
		}
	}

	if ok, _ := keychainHasCert(systemKeychain, m.cert); ok {
		inner := fmt.Sprintf("security delete-certificate -Z %s -t %s", shQuote(m.SHA1()), shQuote(systemKeychain))
		script := fmt.Sprintf("do shell script %s with prompt %s with administrator privileges",
			asQuote(inner), asQuote("观流 需要管理员权限从系统信任库移除根证书"))
		if _, err := runCmd("osascript", "-e", script); err != nil {
			failures = append(failures, "系统钥匙串: "+err.Error())
		} else {
			removed = append(removed, "系统钥匙串")
		}
	}

	if len(failures) > 0 {
		res.Message = "部分位置移除失败"
		res.Output = strings.Join(failures, "\n")
		return res, fmt.Errorf("移除失败：%s", strings.Join(failures, "；"))
	}
	if len(removed) == 0 {
		res.OK = true
		res.Message = "本机信任库中未找到该证书，无需移除"
		return res, nil
	}
	res.OK = true
	res.Message = "已从 " + strings.Join(removed, "、") + " 移除"
	return res, nil
}

// shQuote 用单引号包裹字符串，供 shell 命令安全拼接。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// asQuote 把字符串包成 AppleScript 的双引号字面量。
func asQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
