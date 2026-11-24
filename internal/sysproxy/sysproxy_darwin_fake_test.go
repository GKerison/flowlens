//go:build darwin

package sysproxy

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// fakeSystem 用内存模拟 macOS 的系统网络设置。
//
// 存在的意义：接管/恢复是唯一会让用户断网的路径，
// 而真实执行需要管理员授权、还会改掉开发机自己的代理。
// 用假系统可以把「快照 → 应用 → 校验 → 回滚」整条链路跑通而不碰真实设置。
type fakeSystem struct {
	web     map[string]proxyCfg
	secure  map[string]proxyCfg
	auto    map[string]proxyCfg
	bypass  map[string][]string
	svcList []string
	// ignoreWrites 模拟「命令返回成功但设置没生效」，用于验证回滚
	ignoreWrites bool
}

type proxyCfg struct {
	enabled bool
	server  string
	port    int
}

func newFakeSystem() *fakeSystem {
	const wifi = "Wi-Fi"
	return &fakeSystem{
		web:     map[string]proxyCfg{wifi: {}},
		secure:  map[string]proxyCfg{wifi: {}},
		auto:    map[string]proxyCfg{wifi: {}},
		bypass:  map[string][]string{wifi: {"*.corp.example"}},
		svcList: []string{wifi},
	}
}

// run 是假的命令执行器。
func (f *fakeSystem) run(name string, args []string) (string, error) {
	if strings.HasSuffix(name, "scutil") {
		return f.scutil(), nil
	}
	if !strings.HasSuffix(name, "networksetup") {
		return "", fmt.Errorf("unexpected command: %s", name)
	}
	if len(args) == 0 {
		return "", fmt.Errorf("networksetup 缺少参数")
	}
	svc := ""
	if len(args) > 1 {
		svc = args[1]
	}
	switch args[0] {
	case "-listallnetworkservices":
		return "An asterisk (*) denotes that a network service is disabled.\n" + strings.Join(f.svcList, "\n") + "\n", nil
	case "-getwebproxy":
		return formatProxy(f.web[svc]), nil
	case "-getsecurewebproxy":
		return formatProxy(f.secure[svc]), nil
	case "-getautoproxyurl":
		c := f.auto[svc]
		url := "(null)"
		if c.server != "" {
			url = c.server
		}
		return "URL: " + url + "\nEnabled: " + yesNo(c.enabled) + "\n", nil
	case "-getproxybypassdomains":
		list := f.bypass[svc]
		if len(list) == 0 {
			return "There aren't any bypass domains set on " + svc + ".\n", nil
		}
		return strings.Join(list, "\n") + "\n", nil
	case "-setwebproxy":
		f.set(f.web, svc, args[2], args[3])
	case "-setsecurewebproxy":
		f.set(f.secure, svc, args[2], args[3])
	case "-setwebproxystate":
		f.state(f.web, svc, args[2])
	case "-setsecurewebproxystate":
		f.state(f.secure, svc, args[2])
	case "-setautoproxystate":
		f.state(f.auto, svc, args[2])
	case "-setautoproxyurl":
		if !f.ignoreWrites {
			f.auto[svc] = proxyCfg{server: args[2]}
		}
	case "-setproxybypassdomains":
		if !f.ignoreWrites {
			if len(args) == 3 && args[2] == "Empty" {
				f.bypass[svc] = nil
			} else {
				f.bypass[svc] = append([]string(nil), args[2:]...)
			}
		}
	default:
		return "", fmt.Errorf("unexpected networksetup arg: %s", args[0])
	}
	return "", nil
}

func (f *fakeSystem) set(m map[string]proxyCfg, svc, server, port string) {
	if f.ignoreWrites {
		return
	}
	p, _ := strconv.Atoi(port)
	m[svc] = proxyCfg{enabled: true, server: server, port: p}
}

func (f *fakeSystem) state(m map[string]proxyCfg, svc, v string) {
	if f.ignoreWrites {
		return
	}
	c := m[svc]
	c.enabled = v == "on"
	m[svc] = c
}

func (f *fakeSystem) scutil() string {
	var b strings.Builder
	b.WriteString("<dictionary> {\n")
	for _, pair := range []struct {
		prefix string
		cfg    proxyCfg
	}{{"HTTP", f.web["Wi-Fi"]}, {"HTTPS", f.secure["Wi-Fi"]}} {
		if !pair.cfg.enabled {
			continue
		}
		fmt.Fprintf(&b, "  %sEnable : 1\n  %sProxy : %s\n  %sPort : %d\n",
			pair.prefix, pair.prefix, pair.cfg.server, pair.prefix, pair.cfg.port)
	}
	b.WriteString("}\n")
	return b.String()
}

func formatProxy(c proxyCfg) string {
	return "Enabled: " + yesNo(c.enabled) + "\nServer: " + c.server + "\nPort: " + strconv.Itoa(c.port) + "\nAuthenticated Proxy Enabled: 0\n"
}

func yesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// installFake 把假系统接上，并伪造「已提权」以跳过 osascript。
func installFake(t *testing.T, f *fakeSystem) {
	t.Helper()
	oldRun, oldElevated := runCommand, isElevated
	runCommand = func(name string, args ...string) (string, error) { return f.run(name, args) }
	isElevated = func() bool { return true }
	t.Cleanup(func() { runCommand, isElevated = oldRun, oldElevated })
}

func TestEnableThenDisableRestoresOriginal(t *testing.T) {
	f := newFakeSystem()
	// 原始状态：HTTP 走公司代理，HTTPS 关闭，PAC 关闭
	f.web["Wi-Fi"] = proxyCfg{enabled: true, server: "10.0.0.1", port: 3128}
	installFake(t, f)

	m := New(t.TempDir(), func() string { return "127.0.0.1:8888" })

	res, err := m.Enable()
	if err != nil {
		t.Fatalf("接管失败: %v (%s)", err, res.Output)
	}
	if !res.OK || !res.Changed {
		t.Fatalf("接管结果异常: %+v", res)
	}
	if got := f.web["Wi-Fi"]; !got.enabled || got.server != "127.0.0.1" || got.port != 8888 {
		t.Fatalf("HTTP 代理未指向 观流: %+v", got)
	}
	if got := f.secure["Wi-Fi"]; !got.enabled || got.port != 8888 {
		t.Fatalf("HTTPS 代理未指向 观流: %+v", got)
	}
	// 绕过列表必须包含回环，否则代理会请求自己
	if !contains(f.bypass["Wi-Fi"], "127.0.0.1") {
		t.Fatalf("绕过列表缺少回环地址: %v", f.bypass["Wi-Fi"])
	}
	if st := m.Status(); !st.Enabled {
		t.Fatalf("状态应为已接管: %+v", st)
	}

	// 恢复：必须回到公司代理，而不是简单置空
	res, err = m.Disable()
	if err != nil {
		t.Fatalf("恢复失败: %v (%s)", err, res.Output)
	}
	if got := f.web["Wi-Fi"]; !got.enabled || got.server != "10.0.0.1" || got.port != 3128 {
		t.Fatalf("HTTP 未还原为原代理: %+v", got)
	}
	if got := f.secure["Wi-Fi"]; got.enabled {
		t.Fatalf("HTTPS 原本关闭，应还原为关闭: %+v", got)
	}
	if got := f.bypass["Wi-Fi"]; len(got) != 1 || got[0] != "*.corp.example" {
		t.Fatalf("绕过列表未还原: %v", got)
	}
	if m.loadState() != nil {
		t.Fatal("恢复后应删除接管记录")
	}
}

func TestEnableRollsBackWhenApplySilentlyFails(t *testing.T) {
	f := newFakeSystem()
	f.web["Wi-Fi"] = proxyCfg{enabled: true, server: "10.0.0.1", port: 3128}
	f.ignoreWrites = true // 命令返回成功，但系统没变
	installFake(t, f)

	m := New(t.TempDir(), func() string { return "127.0.0.1:8888" })

	res, err := m.Enable()
	if err == nil {
		t.Fatal("校验未通过时必须报错，而不是宣布成功")
	}
	if res.OK {
		t.Fatalf("失败结果不应标记 ok: %+v", res)
	}
	if !strings.Contains(res.Message, "回滚") {
		t.Fatalf("提示语应说明已回滚: %s", res.Message)
	}
	if m.loadState() != nil {
		t.Fatal("回滚后应清理接管记录，避免下次误判残留")
	}
	if got := f.web["Wi-Fi"]; got.server != "10.0.0.1" {
		t.Fatalf("原代理被破坏: %+v", got)
	}
}

func TestDisableSkipsWhenSystemChangedExternally(t *testing.T) {
	f := newFakeSystem()
	installFake(t, f)

	m := New(t.TempDir(), func() string { return "127.0.0.1:8888" })
	if _, err := m.Enable(); err != nil {
		t.Fatalf("接管失败: %v", err)
	}

	// 模拟用户在系统设置里手动关掉了代理
	f.web["Wi-Fi"] = proxyCfg{}
	f.secure["Wi-Fi"] = proxyCfg{}

	res, err := m.Disable()
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if !strings.Contains(res.Message, "已清理") {
		t.Fatalf("外部修改时应跳过恢复并清理记录: %s", res.Message)
	}
	if got := f.web["Wi-Fi"]; got.enabled {
		t.Fatalf("不应把用户手动关闭的代理重新打开: %+v", got)
	}
	if m.loadState() != nil {
		t.Fatal("应清理接管记录")
	}
}

func TestEnableIsIdempotent(t *testing.T) {
	f := newFakeSystem()
	installFake(t, f)
	m := New(t.TempDir(), func() string { return "127.0.0.1:8888" })

	if _, err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	res, err := m.Enable()
	if err != nil {
		t.Fatalf("重复接管不应失败: %v", err)
	}
	if res.Changed {
		t.Fatal("第二次接管不应再改系统设置")
	}
}

func TestRepointFollowsPortChange(t *testing.T) {
	f := newFakeSystem()
	installFake(t, f)

	port := 8888
	m := New(t.TempDir(), func() string { return "127.0.0.1:" + strconv.Itoa(port) })
	if _, err := m.Enable(); err != nil {
		t.Fatal(err)
	}

	port = 9999
	res, err := m.Repoint()
	if err != nil {
		t.Fatalf("改指失败: %v", err)
	}
	if !res.Changed {
		t.Fatal("端口变化后应重新写入系统设置")
	}
	if got := f.web["Wi-Fi"]; got.port != 9999 {
		t.Fatalf("HTTP 端口未跟随: %+v", got)
	}
	if st := m.loadState(); st == nil || st.ProxyAddr != "127.0.0.1:9999" {
		t.Fatalf("接管记录未更新: %+v", st)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
