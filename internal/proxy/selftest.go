package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SelfTestResult 是"整条链路是否打通"的自检结果。
//
// 判定逻辑刻意设置为「对目标站点做一次**校验证书**的 HTTPS 请求」：
// 只要这次请求成功，就同时证明了三件事——代理在监听、TLS 被解密、
// 客户端信任了 观流 的根证书。任何一环没通都会在这里失败。
type SelfTestResult struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	Hint      string `json:"hint,omitempty"`
	TargetURL string `json:"targetUrl"`
	Status    int    `json:"status"`
	Duration  int64  `json:"durationMs"`
	Flows     int    `json:"flows"` // 自检后列表里的会话总数，用来确认流量真的入库了
}

// DefaultSelfTestTarget 是自检默认访问的站点。
// 选 example.com：长期稳定、无重定向、响应体极小。
const DefaultSelfTestTarget = "https://www.example.com/"

// SelfTest 通过本机代理发起一次 HTTPS 请求，验证解密链路。
// target 为空时使用 DefaultSelfTestTarget。
func (m *Manager) SelfTest(target string) SelfTestResult {
	return m.selfTest(target, nil)
}

// selfTest 是自检的实现，roots 为空表示使用系统信任库。
//
// roots 存在的唯一目的是让测试能注入"已安装该 CA"的等价条件：
// 单元测试无法真的把证书装进开发者机器的钥匙串，但可以传入一张只包含该 CA 的
// 证书池，从而覆盖"信任已生效"这条成功路径。
func (m *Manager) selfTest(target string, roots *x509.CertPool) SelfTestResult {
	if target == "" {
		target = DefaultSelfTestTarget
	}
	res := SelfTestResult{TargetURL: target}

	st := m.State()
	if !st.Running {
		res.Message = "代理未启动，先点「启动代理」"
		res.Hint = "启动代理后再试一次"
		return res
	}

	cfg := m.cfg.Get()
	proxyURL, err := url.Parse("http://" + st.Addr)
	if err != nil {
		res.Message = "代理地址异常：" + err.Error()
		return res
	}
	// 开了代理认证就必须把凭据放进代理 URL，否则自检会拿到 407，
	// 用户会以为是"证书没装好"，排查方向完全错。
	if cfg.RequireAuth && cfg.AuthPassword != "" {
		proxyURL.User = url.UserPassword(cfg.AuthUsername, cfg.AuthPassword)
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		// 关键：不跳过校验。跳过就测不出证书是否真的被信任。
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext:         (&net.Dialer{Timeout: 8 * time.Second}).DialContext,
		TLSHandshakeTimeout: 8 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   12 * time.Second,
		// 不跟随重定向：自检只关心能否建立可信连接
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer transport.CloseIdleConnections()

	before := m.store.Len()
	start := time.Now()
	resp, err := client.Get(target)
	res.Duration = time.Since(start).Milliseconds()
	if err != nil {
		classifySelfTestError(&res, err)
		return res
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	res.Status = resp.StatusCode
	res.Flows = m.store.Len()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		res.Message = "代理要求认证：自检用的用户名或密码不对"
		res.Hint = "在「设置 → 网络与代理 → 代理认证」里核对用户名和密码，保存后重启代理"
		return res
	}
	if resp.StatusCode >= 400 {
		// 代理自己返回 4xx/5xx，基本都发生在上游握手/连接阶段（例如上游自签证书不被信任），
		// 此时客户端侧 TLS 是通的，所以必须按状态码而不是 err 判断。
		res.Message = fmt.Sprintf("代理返回 %d：上游连接失败", resp.StatusCode)
		res.Hint = "若目标站点使用自签证书，可在「设置」中开启「跳过上游证书校验」；否则检查该站点是否可达"
		return res
	}

	res.OK = true
	res.Message = fmt.Sprintf("链路正常：%s 返回 %d，用时 %d ms", strings.TrimSuffix(strings.TrimPrefix(target, "https://"), "/"), resp.StatusCode, res.Duration)
	if res.Flows <= before {
		// 请求通了但列表没新增，说明抓取侧没接上——必须报出来，否则用户会以为"看不到也没事"
		res.OK = false
		res.Message = "请求成功了，但列表里没有新增会话：抓取可能处于暂停状态"
		res.Hint = "检查顶栏是否处于「暂停」状态"
	}
	return res
}

// classifySelfTestError 把底层错误翻译成用户能据此行动的建议。
// 这里不做字符串硬匹配的地方都用 errors.As，避免不同 Go 版本错误文案变化导致误判。
func classifySelfTestError(res *SelfTestResult, err error) {
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var certErr x509.CertificateInvalidError
	var verifyErr *tls.CertificateVerificationError

	switch {
	case errors.As(err, &unknownAuth):
		res.Message = "证书未被信任：本机还不信任 观流 根证书"
		res.Hint = "在上方「本机信任」里点「安装到系统信任」后再试"
	case errors.As(err, &verifyErr), errors.As(err, &certErr):
		res.Message = "证书校验失败：" + err.Error()
		res.Hint = "如果之前重新生成过证书，需要先卸载旧证书再重新安装"
	case errors.As(err, &hostErr):
		res.Message = "证书域名不匹配：" + err.Error()
		res.Hint = "在「设置」里确认解密开关已打开"
	case isConnRefused(err):
		res.Message = "连不上本机代理（" + res.TargetURL + "）"
		res.Hint = "确认代理正在运行，端口未被其他程序占用"
	case isProxyAuth(err):
		// HTTPS 走 CONNECT，认证失败时 Transport 直接报错而不是返回 407 响应
		res.Message = "代理要求认证：用户名或密码不正确"
		res.Hint = "在「设置 → 网络与代理 → 代理认证」里核对用户名和密码，保存后重启代理"
	case isTimeout(err), isDNS(err):
		res.Message = "外网不可达：" + err.Error()
		res.Hint = "自检需要访问外网；离线环境下可改为手动打开任意 HTTPS 网站验证"
	default:
		res.Message = "自检失败：" + err.Error()
	}
}

func isConnRefused(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return strings.Contains(opErr.Err.Error(), "connection refused")
	}
	return strings.Contains(err.Error(), "connection refused")
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	if errors.As(err, &t) {
		return t.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func isDNS(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

// isProxyAuth 判断错误是否来自代理认证失败。
// Go 的 Transport 对 CONNECT 的 407 会包成 url.Error，文案里带状态码或状态文本，
// 两条都匹配以覆盖不同版本。
func isProxyAuth(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Proxy Authentication Required") ||
		strings.Contains(s, "proxyconnect") && strings.Contains(s, "407")
}
