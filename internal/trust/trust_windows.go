//go:build windows

package trust

import (
	"fmt"
	"strings"
)

// currentStore 描述默认安装目标。
// 默认写「当前用户」的根证书库：不需要管理员权限，且对该用户的浏览器与命令行工具都生效。
func currentStore() Store {
	return Store{
		Name:     "受信任的根证书颁发机构（当前用户）",
		Scope:    "user",
		Requires: "",
	}
}

// canElevate 表示此平台支持改用计算机级安装。
func canElevate() bool { return true }

// installCommand 返回可预览的命令行。
// 提权安装用 PowerShell 的 Start-Process -Verb RunAs 触发 UAC。
func installCommand(m *Manager, elevated bool) []string {
	if elevated {
		return []string{"powershell", "-Command",
			fmt.Sprintf(`Start-Process certutil -Verb RunAs -Wait -ArgumentList '-addstore Root "%s"'`, m.certPath)}
	}
	return []string{"certutil", "-user", "-addstore", "Root", m.certPath}
}

// detect 在用户库与计算机库中查找 SHA-1 指纹。
// certutil -store 的输出里每张证书都带一行 "证书哈希(sha1)"，直接搜指纹最稳，
// 不依赖本地化文案（中文/英文系统的 certutil 输出不同）。
func detect(m *Manager) (trusted, elevated bool, detail string, cmd []string) {
	cmd = installCommand(m, false)
	sha1 := m.SHA1()

	if out, err := runCmd("certutil", "-user", "-store", "Root"); err == nil {
		if strings.Contains(strings.ToUpper(out), sha1) {
			return true, false, "已安装到「当前用户」的受信任根证书", cmd
		}
	}
	if out, err := runCmd("certutil", "-store", "Root"); err == nil {
		if strings.Contains(strings.ToUpper(out), sha1) {
			return true, true, "已安装到「本地计算机」的受信任根证书", cmd
		}
	}
	return false, false, "尚未安装到 Windows 证书库，HTTPS 会被浏览器拦截", cmd
}

// install 把证书加入受信任根证书库。
func install(m *Manager, elevated bool) (Result, error) {
	res := Result{Command: installCommand(m, elevated)}

	var out string
	var err error
	if elevated {
		// 通过 PowerShell 触发 UAC；-Wait 保证返回时安装已结束，便于立刻检测
		ps := fmt.Sprintf(`Start-Process certutil -Verb RunAs -Wait -ArgumentList '-addstore Root "%s"'`, m.certPath)
		out, err = runCmd("powershell", "-NoProfile", "-Command", ps)
	} else {
		out, err = runCmd("certutil", "-user", "-addstore", "Root", m.certPath)
	}
	if err != nil {
		res.Message = "安装失败"
		res.Output = out
		return res, err
	}

	trusted, _, detail, _ := detect(m)
	res.OK = trusted
	res.Message = detail
	if !trusted {
		return res, errNotApplied
	}
	return res, nil
}

// uninstall 从两个证书库中移除本证书（按 SHA-1 指纹精确删除）。
func uninstall(m *Manager) (Result, error) {
	res := Result{Command: []string{"certutil", "-user", "-delstore", "Root", m.SHA1()}}

	trusted, elevated, _, _ := detect(m)
	if !trusted {
		res.OK = true
		res.Message = "Windows 证书库中未找到该证书，无需移除"
		return res, nil
	}

	if elevated {
		// 只装在计算机库：走提权删除
		ps := fmt.Sprintf("Start-Process certutil -Verb RunAs -Wait -ArgumentList '-delstore Root %s'", m.SHA1())
		if out, err := runCmd("powershell", "-NoProfile", "-Command", ps); err != nil {
			res.Message = "本地计算机证书库移除失败"
			res.Output = out
			return res, err
		}
		res.OK = true
		res.Message = "已从「本地计算机」证书库移除"
		return res, nil
	}

	if out, err := runCmd("certutil", "-user", "-delstore", "Root", m.SHA1()); err != nil {
		res.Message = "当前用户证书库移除失败"
		res.Output = out
		return res, err
	}
	res.OK = true
	res.Message = "已从「当前用户」证书库移除"
	return res, nil
}
