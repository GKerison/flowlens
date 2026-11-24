package service_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/proxy"
	"github.com/kerison/flowlens/internal/service"
)

// 服务层是前端唯一入口，这里的用例覆盖"界面点了保存之后到底会发生什么"。

// newTestApp 装配一个最小可用的服务对象：配置与 CA 落在临时目录，系统代理不装配。
func newTestApp(t *testing.T) (*service.App, *config.Manager) {
	t.Helper()
	app, cfg, _ := newTestAppWithStore(t)
	return app, cfg
}

// newTestAppWithStore 同上，额外返回存储，便于构造"已有会话"的场景。
func newTestAppWithStore(t *testing.T) (*service.App, *config.Manager, *capture.Store) {
	t.Helper()

	dir := t.TempDir()
	cfg, err := config.NewManagerAt(dir)
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	// 端口交给系统分配，避免与开发机上正在跑的代理撞车
	port := freePortOrFatal(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ListenHost = "127.0.0.1"
		c.ProxyPort = port
		return nil
	}); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	caMgr, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	store := capture.NewStore(100)
	mgr := proxy.NewManager(cfg, caMgr, store, nil)
	t.Cleanup(func() { _ = mgr.Stop() })

	app := service.New(service.Deps{Config: cfg, CA: caMgr, Store: store, Proxy: mgr})
	return app, cfg, store
}

func freePortOrFatal(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestSaveConfigRejectsAuthWithoutPassword 锁定"不能假装安全"：
// 勾了认证不填密码必须报错，且配置不能被改动。
func TestSaveConfigRejectsAuthWithoutPassword(t *testing.T) {
	app, cfg := newTestApp(t)
	next := cfg.Get()
	next.RequireAuth = true
	next.AuthPassword = ""

	if _, err := app.SaveConfig(next); err == nil {
		t.Fatal("空密码的认证配置应被拒绝")
	}
	if got := app.GetConfig(); got.RequireAuth {
		t.Fatal("被拒绝的配置不应写入")
	}
}

// TestSaveConfigAuthAppliesImmediately 验证"改完认证不用重启代理"：
// 保存后旧的匿名请求应立刻被 407 拒绝，带凭据的请求正常。
func TestSaveConfigAuthAppliesImmediately(t *testing.T) {
	app, cfg := newTestApp(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	if _, err := app.StartProxy(); err != nil {
		t.Fatalf("启动代理失败: %v", err)
	}
	t.Cleanup(func() { _, _ = app.StopProxy() })

	addr := net.JoinHostPort(cfg.Get().ListenHost, strconv.Itoa(cfg.Get().ProxyPort))
	get := func(proxyAddr string) (*http.Response, error) {
		u, err := url.Parse(proxyAddr)
		if err != nil {
			t.Fatalf("解析代理地址失败: %v", err)
		}
		tr := &http.Transport{Proxy: http.ProxyURL(u)}
		defer tr.CloseIdleConnections()
		return (&http.Client{Timeout: 5 * time.Second, Transport: tr}).Get(upstream.URL + "/svc")
	}

	// 未开认证：匿名可通
	resp, err := get("http://" + addr)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("未开认证时应正常转发：err=%v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	next := cfg.Get()
	next.RequireAuth = true
	next.AuthUsername = "svc"
	next.AuthPassword = "pw123456"
	if _, err := app.SaveConfig(next); err != nil {
		t.Fatalf("保存认证配置失败: %v", err)
	}

	resp, err = get("http://" + addr)
	if err != nil {
		t.Fatalf("匿名请求应返回 407 而不是连接错误: %v", err)
	}
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("保存后应立即要求认证，实际 %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp, err = get("http://svc:pw123456@" + addr)
	if err != nil {
		t.Fatalf("带凭据请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带凭据应放行，实际 %d", resp.StatusCode)
	}

	// 代理仍在运行：认证热生效不等于重启
	if !app.ProxyState().Running {
		t.Fatal("保存配置不应导致代理停止")
	}
}

// TestBootstrapReportsLANMode 覆盖界面用到的两个判断依据。
func TestBootstrapReportsLANMode(t *testing.T) {
	app, cfg := newTestApp(t)

	bs := app.Bootstrap()
	if bs.Cert.LAN {
		t.Fatal("监听 127.0.0.1 时不应报告局域网模式")
	}
	if bs.ProxyURL != "http://"+net.JoinHostPort(cfg.Get().ListenHost, strconv.Itoa(cfg.Get().ProxyPort)) {
		t.Fatalf("代理地址不正确：%s", bs.ProxyURL)
	}

	next := cfg.Get()
	next.ListenHost = "0.0.0.0"
	if _, err := app.SaveConfig(next); err != nil {
		t.Fatalf("切换监听地址失败: %v", err)
	}
	// 证书服务未装配时 CertServer 会返回"未初始化"，这里只看 LAN 判定
	if !app.CertServer().LAN {
		t.Fatal("监听 0.0.0.0 时应报告局域网模式")
	}
	// 0.0.0.0 是监听地址，不是可填的代理地址：界面必须给出本机可用的 127.0.0.1
	if got := app.Bootstrap().ProxyURL; got != "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Get().ProxyPort)) {
		t.Fatalf("0.0.0.0 模式下的代理地址应回落为本机地址，实际 %s", got)
	}
}

// TestSaveConfigRejectsBrokenMapLocalRule 锁定"规则写了就必须真能用"：
// 启用但文件不存在的规则如果被静默接受，用户会以为 mock 生效，
// 实际却一直拿到上游响应，排查成本极高。
func TestSaveConfigRejectsBrokenMapLocalRule(t *testing.T) {
	app, cfg := newTestApp(t)
	next := cfg.Get()
	next.MapLocalRules = []config.MapLocalRule{{
		Enabled: true,
		Pattern: "/api/users",
		File:    filepath.Join(t.TempDir(), "missing.json"),
	}}

	if _, err := app.SaveConfig(next); err == nil {
		t.Fatal("文件不存在的映射规则应被拒绝")
	}
	if len(app.GetConfig().MapLocalRules) != 0 {
		t.Fatal("被拒绝的配置不应写入")
	}

	// 正则写错同样要拦：否则规则永远匹配不上，比报错更难发现
	next.MapLocalRules[0].File = writeTempJSON(t, `{}`)
	next.MapLocalRules[0].Pattern = "re:([a-z"
	if _, err := app.SaveConfig(next); err == nil {
		t.Fatal("非法正则应被拒绝")
	}
}

// TestExportJSONWritesFile 验证"点了导出之后文件确实出现在下载目录"。
// 把 HOME 指到临时目录，避免污染开发机的 Downloads。
func TestExportJSONWritesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "Downloads"), 0o755); err != nil {
		t.Fatalf("创建下载目录失败: %v", err)
	}

	app, _, store := newTestAppWithStore(t)
	if _, err := app.ExportJSON(nil, ""); err == nil {
		t.Fatal("没有会话时应报错")
	}

	store.Add(&capture.FlowDetail{
		FlowBrief: capture.FlowBrief{
			ID: "svc-1", Method: "GET", Scheme: "https", Host: "api.example.com",
			Path: "/v1/ping", StatusCode: 200, StartTime: time.Now(),
		},
		ReqHeaders: http.Header{"X-Test": []string{"1"}},
	})

	res, err := app.ExportJSON(nil, "")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if res.Entries != 1 {
		t.Fatalf("应导出 1 条，实际 %d", res.Entries)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("读取导出文件失败: %v", err)
	}
	if !strings.Contains(string(data), capture.JSONFormat) {
		t.Fatalf("导出内容缺少格式标识：%s", string(data[:min(len(data), 120)]))
	}
}

// TestExportToChosenPath 验证"另存为"路径：用户的路径优先于下载目录。
// 三个易错点必须钉住：
//  1. 没有扩展名时补 .json（否则脚本按后缀识别文件类型会失败）；
//  2. 已有其他扩展名时原样保留（不制造 trace.txt.json）；
//  3. 目录不存在时报错而不是静默写到别处。
func TestExportToChosenPath(t *testing.T) {
	// 把 HOME 指向一个没有 Downloads 的临时目录：如果实现偷偷回退到下载目录，这里会写到临时目录里，
	// 断言路径相等时就能发现
	t.Setenv("HOME", t.TempDir())
	app, _, store := newTestAppWithStore(t)
	store.Add(&capture.FlowDetail{
		FlowBrief: capture.FlowBrief{
			ID: "svc-path", Method: "GET", Scheme: "https", Host: "api.example.com",
			Path: "/v1/ping", StatusCode: 200, StartTime: time.Now(),
		},
	})

	dir := t.TempDir()
	res, err := app.ExportJSON(nil, filepath.Join(dir, "trace"))
	if err != nil {
		t.Fatalf("按指定路径导出失败: %v", err)
	}
	if want := filepath.Join(dir, "trace.json"); res.Path != want {
		t.Fatalf("路径应为 %s，实际 %s", want, res.Path)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}

	keep := filepath.Join(dir, "trace.txt")
	if res, err = app.ExportJSON(nil, keep); err != nil {
		t.Fatalf("按指定扩展名导出失败: %v", err)
	}
	if res.Path != keep {
		t.Fatalf("已有扩展名不应被改写，实际 %s", res.Path)
	}

	if _, err := app.ExportJSON(nil, filepath.Join(dir, "no-such-dir", "x.json")); err == nil {
		t.Fatal("目录不存在时应报错")
	}
	if _, err := app.ExportHAR(nil, dir); err == nil {
		t.Fatal("目标是一个目录时应报错")
	}
}

// TestExportCAWritesRestrictedFile 验证 p12 落盘时权限是 0600：
// 私钥文件被同机其他用户读到，等价于把解密能力送出去。
func TestExportCAWritesRestrictedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "Downloads"), 0o755); err != nil {
		t.Fatalf("创建下载目录失败: %v", err)
	}

	app, _ := newTestApp(t)
	if _, err := app.ExportCA("short"); err == nil {
		t.Fatal("短密码应被拒绝")
	}

	res, err := app.ExportCA("flowlens-test-1")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	st, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("导出文件不存在: %v", err)
	}
	if res.Bytes == 0 || st.Size() != res.Bytes {
		t.Fatalf("文件大小不一致：%d vs %d", st.Size(), res.Bytes)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("私钥文件权限应为 0600，实际 %o", st.Mode().Perm())
	}
}

func writeTempJSON(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mock.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时文件失败: %v", err)
	}
	return path
}
