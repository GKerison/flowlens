package sysproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSplitTarget(t *testing.T) {
	cases := []struct {
		in     string
		host   string
		port   int
		hasErr bool
	}{
		{"127.0.0.1:8888", "127.0.0.1", 8888, false},
		{"localhost:1", "localhost", 1, false},
		{"[::1]:8888", "::1", 8888, false},
		{"127.0.0.1", "", 0, true},
		{"127.0.0.1:0", "", 0, true},
		{"127.0.0.1:70000", "", 0, true},
		{":8888", "", 0, true},
	}
	for _, c := range cases {
		host, port, err := splitTarget(c.in)
		if c.hasErr {
			if err == nil {
				t.Fatalf("%q 应报错，实际得到 %s:%d", c.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q 解析失败: %v", c.in, err)
		}
		if host != c.host || port != c.port {
			t.Fatalf("%q 解析为 %s:%d，期望 %s:%d", c.in, host, port, c.host, c.port)
		}
	}
}

func TestPointsToUs(t *testing.T) {
	const target = "127.0.0.1:8888"
	cases := []struct {
		name   string
		mode   string
		addr   string
		target string
		want   bool
	}{
		{"完全一致", "manual", "127.0.0.1:8888", target, true},
		{"localhost 等价", "manual", "localhost:8888", target, true},
		{"端口不同", "manual", "127.0.0.1:8889", target, false},
		{"别人的代理", "manual", "10.0.0.1:3128", target, false},
		{"未启用", "none", "127.0.0.1:8888", target, false},
		{"PAC 模式", "auto", "", target, false},
		{"空地址", "manual", "", target, false},
		{"IPv6 回环", "manual", "[::1]:8888", target, true},
	}
	for _, c := range cases {
		if got := pointsToUs(c.mode, c.addr, c.target); got != c.want {
			t.Fatalf("%s: pointsToUs(%q,%q) = %v，期望 %v", c.name, c.mode, c.addr, got, c.want)
		}
	}
}

func TestFormatCommands(t *testing.T) {
	got := formatCommands([][]string{
		{"networksetup", "-setwebproxy", "Wi-Fi", "127.0.0.1", "8888"},
		{"networksetup", "-setproxybypassdomains", "USB LAN", "localhost"},
	})
	if len(got) != 2 {
		t.Fatalf("期望 2 条命令，实际 %d", len(got))
	}
	if got[0] != "networksetup -setwebproxy Wi-Fi 127.0.0.1 8888" {
		t.Fatalf("命令格式错误: %s", got[0])
	}
	// 带空格的服务名必须加引号，否则复制出去就是错的
	if got[1] != `networksetup -setproxybypassdomains "USB LAN" localhost` {
		t.Fatalf("含空格参数未加引号: %s", got[1])
	}
}

func TestDescribeProxy(t *testing.T) {
	cases := map[string]string{
		"none":        "直连",
		"manual":      "手动 127.0.0.1:8888",
		"auto":        "自动配置脚本（PAC）",
		"unsupported": "系统不支持",
	}
	for mode, want := range cases {
		addr := ""
		if mode == "manual" {
			addr = "127.0.0.1:8888"
		}
		if got := describeProxy(mode, addr); got != want {
			t.Fatalf("describeProxy(%q) = %q，期望 %q", mode, got, want)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, func() string { return "127.0.0.1:8888" })

	st := &stateFile{
		Version:   stateVersion,
		Platform:  "test",
		ProxyAddr: "127.0.0.1:8888",
		Snapshot:  json.RawMessage(`{"k":"v"}`),
	}
	if err := m.writeState(st); err != nil {
		t.Fatalf("写状态失败: %v", err)
	}

	// 状态文件里有用户原始代理配置，权限必须是 0600
	info, err := os.Stat(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatalf("状态文件不存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("状态文件权限为 %o，期望 600", perm)
	}

	got := m.loadState()
	if got == nil || got.ProxyAddr != st.ProxyAddr {
		t.Fatalf("状态回读不一致: %+v", got)
	}

	if err := m.clearState(); err != nil {
		t.Fatalf("清理状态失败: %v", err)
	}
	if g := m.loadState(); g != nil {
		t.Fatal("清理后仍能读到状态")
	}
}

func TestLoadStateIgnoresCorrupted(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, func() string { return "127.0.0.1:8888" })
	if err := os.WriteFile(m.StatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 损坏的状态文件不能让应用起不来，只能当作"没有残留"
	if st := m.loadState(); st != nil {
		t.Fatalf("损坏的状态文件应返回 nil，实际 %+v", st)
	}

	if err := os.WriteFile(m.StatePath(), []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := m.loadState(); st != nil {
		t.Fatal("版本不匹配的状态文件应返回 nil")
	}
}

func TestBootRecoverNoState(t *testing.T) {
	m := New(t.TempDir(), func() string { return "127.0.0.1:8888" })
	res := m.BootRecover()
	if res.Leftover || res.Restored || res.Note != "" {
		t.Fatalf("无状态文件时不应有任何动作: %+v", res)
	}
}
