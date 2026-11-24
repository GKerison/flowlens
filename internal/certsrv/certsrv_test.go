package certsrv

import (
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/ca"
)

// newTestOptions 生成一份真实的根证书作为测试输入。
func newTestOptions(t *testing.T, port int) Options {
	t.Helper()
	caMgr, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("生成测试 CA 失败: %v", err)
	}
	info := caMgr.Info()
	return Options{
		Host:        "127.0.0.1",
		Port:        port,
		CertPEM:     []byte(info.PEM),
		CertDER:     caMgr.GetRootCA().Raw,
		CommonName:  info.CommonName,
		Fingerprint: info.Fingerprint,
		NotAfter:    info.NotAfter,
		Version:     "test",
	}
}

func TestEndpoints(t *testing.T) {
	opts := newTestOptions(t, 0)
	s := New(opts)
	h := s.handler()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// 下载页
	page := get("/")
	if page.Code != http.StatusOK {
		t.Fatalf("下载页状态码 %d", page.Code)
	}
	if ct := page.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("下载页 Content-Type 不对: %s", ct)
	}
	body := page.Body.String()
	for _, want := range []string{opts.CommonName, "/ca.crt", "/ca.mobileconfig", "证书信任设置"} {
		if !strings.Contains(body, want) {
			t.Fatalf("下载页缺少内容 %q", want)
		}
	}
	// 页面必须移动端可用，否则手机打开是缩小版桌面页
	if !strings.Contains(body, `name="viewport"`) {
		t.Fatal("下载页缺少 viewport meta")
	}

	// DER 证书：Content-Type 决定手机是否弹出「安装证书」
	crt := get("/ca.crt")
	if crt.Code != http.StatusOK {
		t.Fatalf("ca.crt 状态码 %d", crt.Code)
	}
	if ct := crt.Header().Get("Content-Type"); ct != "application/x-x509-ca-cert" {
		t.Fatalf("ca.crt Content-Type 不对: %s", ct)
	}
	if !strings.Contains(crt.Header().Get("Content-Disposition"), "flowlens-ca.crt") {
		t.Fatalf("ca.crt 文件名不对: %s", crt.Header().Get("Content-Disposition"))
	}
	if crt.Body.Len() != len(opts.CertDER) {
		t.Fatalf("ca.crt 长度 %d，期望 %d", crt.Body.Len(), len(opts.CertDER))
	}

	// PEM 证书
	pemRes := get("/ca.pem")
	if !strings.Contains(pemRes.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatal("ca.pem 内容不是 PEM")
	}

	// 描述文件
	mc := get("/ca.mobileconfig")
	if mc.Code != http.StatusOK {
		t.Fatalf("mobileconfig 状态码 %d", mc.Code)
	}
	if ct := mc.Header().Get("Content-Type"); ct != "application/x-apple-aspen-config" {
		t.Fatalf("mobileconfig Content-Type 不对: %s", ct)
	}

	// 健康检查与 404
	if ping := get("/ping"); ping.Body.String() != "flowlens" {
		t.Fatalf("ping 返回 %q", ping.Body.String())
	}
	if nf := get("/etc/passwd"); nf.Code != http.StatusNotFound {
		t.Fatalf("未知路径应 404，实际 %d", nf.Code)
	}
}

func TestMobileconfigStructure(t *testing.T) {
	opts := newTestOptions(t, 0)
	body, err := buildMobileconfig(opts)
	if err != nil {
		t.Fatalf("生成描述文件失败: %v", err)
	}
	text := string(body)

	// 必须是合法 XML
	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(body, &probe); err != nil {
		t.Fatalf("描述文件不是合法 XML: %v", err)
	}

	for _, want := range []string{
		"<key>PayloadType</key>",
		"com.apple.security.root",
		"<string>Configuration</string>",
		"com.flowlens.profile.",
		opts.CommonName,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("描述文件缺少 %q", want)
		}
	}

	// 证书载荷必须是 DER 的 base64，能被解开且与原证书一致
	start := strings.Index(text, "<data>")
	end := strings.Index(text, "</data>")
	if start < 0 || end < 0 || end < start {
		t.Fatal("描述文件缺少 <data> 证书载荷")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text[start+len("<data>") : end]))
	if err != nil {
		t.Fatalf("证书载荷不是合法 base64: %v", err)
	}
	if string(raw) != string(opts.CertDER) {
		t.Fatal("证书载荷与源证书不一致")
	}

	// 同一张证书生成的 UUID 必须稳定，否则重复下载会装出多份描述文件
	again, err := buildMobileconfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != text {
		t.Fatal("描述文件生成结果不稳定")
	}
}

func TestStartStop(t *testing.T) {
	opts := newTestOptions(t, 0) // 端口 0 由系统分配，避免与本机其他服务冲突
	s := New(opts)

	addr, err := s.Start()
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if addr == "" || !s.Running() {
		t.Fatal("启动后应处于运行状态并返回地址")
	}

	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("请求证书服务失败: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "flowlens" {
		t.Fatalf("ping 返回 %q", string(b))
	}

	// 重复 Start 不应报错，也不应换地址
	addr2, err := s.Start()
	if err != nil {
		t.Fatalf("重复启动失败: %v", err)
	}
	if addr2 != addr {
		t.Fatalf("重复启动后地址变化：%s → %s", addr, addr2)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if s.Running() {
		t.Fatal("停止后不应处于运行状态")
	}
	// 停止后端口应释放：再启动一次仍能成功
	if _, err := s.Start(); err != nil {
		t.Fatalf("停止后重新启动失败: %v", err)
	}
	_ = s.Stop()
}

func TestStartRejectsEmptyCert(t *testing.T) {
	s := New(Options{Host: "127.0.0.1", Port: 0})
	if _, err := s.Start(); err == nil {
		t.Fatal("没有证书内容时应拒绝启动")
	}
}

func TestShortFingerprint(t *testing.T) {
	fp := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	if got := shortFingerprint(fp); !strings.HasPrefix(got, "AA:BB:CC:DD") || !strings.HasSuffix(got, "…") {
		t.Fatalf("短指纹格式不对: %s", got)
	}
	if got := shortFingerprint("short"); got != "short" {
		t.Fatalf("段数不足时应原样返回，实际 %s", got)
	}
}

func TestProfileUUIDStableAndV4Shaped(t *testing.T) {
	der := []byte("some certificate bytes")
	a := profileUUID(der)
	b := profileUUID(der)
	if a != b {
		t.Fatal("同一证书的 UUID 必须稳定")
	}
	if len(a) != 36 {
		t.Fatalf("UUID 长度应为 36，实际 %d: %s", len(a), a)
	}
	if a[14] != '4' {
		t.Fatalf("UUID 版本位应为 4: %s", a)
	}
	if c := profileUUID([]byte("other")); c == a {
		t.Fatal("不同证书应得到不同 UUID")
	}
}

func TestPageDataUsesRequestHost(t *testing.T) {
	opts := newTestOptions(t, 8889)
	s := New(opts)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.23:51234"
	data := s.pageData(req)
	if data.BaseURL != "http://192.168.1.23:8889" {
		t.Fatalf("下载页应展示实际访问地址，实际 %s", data.BaseURL)
	}
	if data.NotAfter == "" || data.ShortFingerID == "" {
		t.Fatal("下载页数据不完整")
	}
}

func TestTimeFormatting(t *testing.T) {
	opts := newTestOptions(t, 0)
	opts.NotAfter = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	s := New(opts)
	data := s.pageData(httptest.NewRequest(http.MethodGet, "/", nil))
	if data.NotAfter != "2030-01-02" {
		t.Fatalf("到期日格式不对: %s", data.NotAfter)
	}
}
