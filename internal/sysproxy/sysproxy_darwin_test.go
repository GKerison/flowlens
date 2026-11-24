//go:build darwin

package sysproxy

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// fakeRunner 替换命令执行入口，记录调用并返回预置输出。
// 平台测试绝不能真的去改系统网络设置，因此所有用例都必须先装它。
func fakeRunner(t *testing.T, respond func(name string, args []string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	old := runCommand
	runCommand = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if respond == nil {
			return "", nil
		}
		return respond(name, args)
	}
	t.Cleanup(func() { runCommand = old })
	return &calls
}

// 两个启用服务 + 一个被禁用服务：禁用服务不该被接管。
const testServiceList = `An asterisk (*) denotes that a network service is disabled.
Wi-Fi
USB 10/100/1000 LAN
*Bluetooth PAN
`

func TestDarwinApplyCommands(t *testing.T) {
	calls := fakeRunner(t, func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "networksetup") && len(args) > 0 && args[0] == "-listallnetworkservices" {
			return testServiceList, nil
		}
		return "", nil
	})

	cmds, err := applyProxy("127.0.0.1:8888", BypassHosts, execMode{AllowPrompt: false})
	if err == nil && os.Geteuid() != 0 {
		t.Fatal("非 root 且不允许弹框时，应该返回权限错误而不是静默成功")
	}
	if err != nil && !errors.Is(err, ErrPermission) && os.Geteuid() != 0 {
		t.Fatalf("应返回 ErrPermission，实际: %v", err)
	}

	if len(cmds) != 8 {
		t.Fatalf("两个服务各 4 条命令，期望 8 条，实际 %d：%v", len(cmds), formatCommands(cmds))
	}
	want := []string{networksetup, "-setwebproxy", "Wi-Fi", "127.0.0.1", "8888"}
	if strings.Join(cmds[0], " ") != strings.Join(want, " ") {
		t.Fatalf("首条命令错误: %v", cmds[0])
	}
	// 禁用中的服务不能被写入
	for _, argv := range cmds {
		for _, a := range argv {
			if a == "*Bluetooth PAN" {
				t.Fatalf("不应接管已禁用的网络服务: %v", argv)
			}
		}
	}
	// 必须关掉 PAC，否则它会盖掉手动代理
	found := false
	for _, argv := range cmds {
		if len(argv) > 2 && argv[1] == "-setautoproxystate" && argv[3] == "off" {
			found = true
		}
	}
	if !found {
		t.Fatal("缺少关闭 PAC 的命令")
	}
	if len(*calls) == 0 || !strings.Contains((*calls)[0], "-listallnetworkservices") {
		t.Fatalf("未先读取服务列表: %v", *calls)
	}
}

func TestDarwinRestoreCommands(t *testing.T) {
	fakeRunner(t, nil)

	snap := darwinSnapshot{Services: []darwinService{
		{
			Name: "Wi-Fi", WebEnabled: true, WebServer: "10.0.0.1", WebPort: 3128,
			SecureEnabled: true, SecureServer: "10.0.0.1", SecurePort: 3128,
			AutoEnabled: false, Bypass: []string{"*.corp.example", "10.0.0.0/8"},
		},
		{
			Name:   "USB 10/100/1000 LAN", // 原本就是关闭状态，恢复时必须写回 off
			Bypass: nil,
		},
	}}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	st := &stateFile{Version: stateVersion, ProxyAddr: "127.0.0.1:8888", Snapshot: raw}

	cmds, _ := restoreProxy(st, nil, execMode{})

	joined := formatCommands(cmds)
	assertHas := func(want string) {
		for _, line := range joined {
			if line == want {
				return
			}
		}
		t.Fatalf("缺少命令: %s\n实际:\n%s", want, strings.Join(joined, "\n"))
	}
	assertHas(networksetup + " -setwebproxy Wi-Fi 10.0.0.1 3128")
	assertHas(networksetup + " -setsecurewebproxy Wi-Fi 10.0.0.1 3128")
	assertHas(networksetup + ` -setproxybypassdomains Wi-Fi "*.corp.example" 10.0.0.0/8`)
	assertHas(networksetup + ` -setwebproxystate "USB 10/100/1000 LAN" off`)
	// 空绕过列表必须显式写 Empty，否则旧的绕过项会残留
	assertHas(networksetup + ` -setproxybypassdomains "USB 10/100/1000 LAN" Empty`)
}

func TestDarwinRestoreWithoutSnapshot(t *testing.T) {
	fakeRunner(t, func(name string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "-listallnetworkservices" {
			return "Wi-Fi\n", nil
		}
		return "", nil
	})
	cmds, _ := restoreProxy(&stateFile{Version: stateVersion, Preexisting: true}, nil, execMode{})
	joined := strings.Join(formatCommands(cmds), "\n")
	if !strings.Contains(joined, "-setwebproxystate Wi-Fi off") {
		t.Fatalf("无快照时应退化为关闭代理，实际: %s", joined)
	}
}

// 提权路径最容易出错：服务名里有空格或单引号时，拼出来的 shell / AppleScript 必须仍然正确。
// 这里只验证脚本字符串，不真的执行。
func TestDarwinElevatedScriptQuoting(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下不会走 osascript 分支")
	}
	var script string
	fakeRunner(t, func(name string, args []string) (string, error) {
		if name == "osascript" && len(args) >= 2 {
			script = args[1]
		}
		return "", nil
	})

	cmds := [][]string{{networksetup, "-setwebproxy", "Bob's Wi-Fi", "127.0.0.1", "8888"}}
	if err := runCommands(cmds, execMode{AllowPrompt: true}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(script, "with administrator privileges") {
		t.Fatalf("未走提权分支: %s", script)
	}
	// asQuote 会把 shQuote 产生的反斜杠再转义一次，最终 AppleScript 字面量里是 \\
	if !strings.Contains(script, `'Bob'\\''s Wi-Fi'`) {
		t.Fatalf("单引号未转义，脚本会被截断: %s", script)
	}
	if strings.Contains(script, "\n") {
		t.Fatalf("提权脚本不应包含换行: %s", script)
	}

	// 不允许弹框时必须直接失败，不能悄悄调用 osascript
	script = ""
	if err := runCommands(cmds, execMode{AllowPrompt: false}); !errors.Is(err, ErrPermission) {
		t.Fatalf("AllowPrompt=false 时应返回 ErrPermission，实际: %v", err)
	}
	if script != "" {
		t.Fatalf("AllowPrompt=false 时不应调用 osascript: %s", script)
	}
}

func TestDarwinParseKV(t *testing.T) {
	// 真实输出里 "Server: " 后面是空，且键值之间是 " : "
	out := `<dictionary> {
  HTTPEnable : 1
  HTTPProxy : 127.0.0.1
  HTTPPort : 8888
  ExceptionsList : <array> {
    0 : *.local
  }
}
`
	kv := parseKV(out)
	if kv["HTTPEnable"] != "1" || kv["HTTPProxy"] != "127.0.0.1" || kv["HTTPPort"] != "8888" {
		t.Fatalf("解析结果错误: %+v", kv)
	}

	// networksetup 的格式是 "Key: Value"，冒号后只有一个空格
	ns := "Enabled: No\nServer: \nPort: 0\n"
	kv = parseKV(ns)
	if kv["Enabled"] != "No" || kv["Server"] != "" || kv["Port"] != "0" {
		t.Fatalf("networksetup 输出解析错误: %+v", kv)
	}
}

func TestDarwinParseBool(t *testing.T) {
	for _, s := range []string{"1", "Yes", "yes", "on", "TRUE"} {
		if !parseBool(s) {
			t.Fatalf("%q 应识别为真", s)
		}
	}
	for _, s := range []string{"0", "No", "", "off"} {
		if parseBool(s) {
			t.Fatalf("%q 应识别为假", s)
		}
	}
}

// scutilOutput 生成一份 scutil --proxy 的假输出。
func scutilOutput(host string, port int) string {
	return "<dictionary> {\n" +
		"  HTTPEnable : 1\n" +
		"  HTTPPort : " + itoa(port) + "\n" +
		"  HTTPProxy : " + host + "\n" +
		"  HTTPSEnable : 1\n" +
		"  HTTPSPort : " + itoa(port) + "\n" +
		"  HTTPSProxy : " + host + "\n" +
		"}\n"
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestDarwinBootRecoverNotOurs(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, func() string { return "127.0.0.1:8888" })

	fakeRunner(t, func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "scutil") {
			return scutilOutput("10.0.0.5", 3128), nil
		}
		return "", nil
	})

	st := &stateFile{Version: stateVersion, ProxyAddr: "127.0.0.1:8888", Snapshot: json.RawMessage(`{}`)}
	if err := m.writeState(st); err != nil {
		t.Fatal(err)
	}

	res := m.BootRecover()
	if res.Leftover || res.Restored {
		t.Fatalf("系统代理已不是我们时不应报残留: %+v", res)
	}
	if !strings.Contains(res.Note, "已清理") {
		t.Fatalf("提示语不对: %s", res.Note)
	}
	if m.loadState() != nil {
		t.Fatal("应清理残留状态文件")
	}
}

func TestDarwinBootRecoverLeftoverNeedsAdmin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 环境下会自动恢复，不适用本用例")
	}
	dir := t.TempDir()
	// 用一个几乎不可能在监听的高位端口，避免命中"端口仍在监听"分支
	const addr = "127.0.0.1:59721"
	m := New(dir, func() string { return addr })

	calls := fakeRunner(t, func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "scutil") {
			return scutilOutput("127.0.0.1", 59721), nil
		}
		return "", nil
	})

	st := &stateFile{Version: stateVersion, ProxyAddr: addr, Snapshot: json.RawMessage(`{"services":[]}`)}
	if err := m.writeState(st); err != nil {
		t.Fatal(err)
	}

	res := m.BootRecover()
	if !res.Leftover {
		t.Fatalf("应识别出残留: %+v", res)
	}
	if res.Restored {
		t.Fatal("需要管理员权限时不应声称已恢复")
	}
	if !strings.Contains(res.Note, "管理员") {
		t.Fatalf("提示语应说明需要管理员权限: %s", res.Note)
	}
	if m.loadState() == nil {
		t.Fatal("恢复失败时不能删除状态文件，否则下次无法再恢复")
	}
	// 不允许在启动阶段弹授权框
	for _, c := range *calls {
		if strings.Contains(c, "osascript") {
			t.Fatalf("启动自愈不应弹授权框: %s", c)
		}
	}

	if st := m.Status(); !st.Stale || !st.Enabled {
		t.Fatalf("状态应为「残留且仍指向我们」: %+v", st)
	}
}
