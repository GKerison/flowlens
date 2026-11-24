// Package replay 实现请求重放：用已捕获的请求内容重新发出一次真实请求。
//
// 设计取舍：
//   - 重放不走代理内核，而是直接发起 HTTP 请求。这样即使代理已停止也能用，
//     且不会因为重放流量再次进入抓包列表而互相干扰；
//   - 不跟随重定向（返回 ErrUseLastResponse），否则用户看不到 302/Location 这些关键信息；
//   - 上游代理与 TLS 校验策略沿用当前配置，保证"公司网络下也能重放"。
package replay

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kerison/flowlens/internal/capture"
)

// Options 是重放所需的网络参数，由调用方从配置中读取后传入。
type Options struct {
	UpstreamProxy string        // 上游代理，空表示直连
	Insecure      bool          // 跳过 TLS 校验（调试自签服务时使用）
	Timeout       time.Duration // 单次重放超时，<=0 时用 30s
	MaxBodyBytes  int64         // 响应体保留上限，<=0 时用 2MB
}

// hopHeaders 是逐跳头部，跨连接转发时会误导下游，必须剔除。
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length", "Accept-Encoding",
}

// Replayer 执行重放。可复用，内部持有的 Transport 会复用连接。
type Replayer struct {
	client *http.Client
	max    int64
}

// New 创建重放器。
func New(opts Options) (*Replayer, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	max := opts.MaxBodyBytes
	if max <= 0 {
		max = 2 << 20
	}

	transport := &http.Transport{
		// 重放的响应体大小已知可控，保持连接复用能显著加快连续重放
		Proxy:               func(*http.Request) (*url.URL, error) { return parseProxy(opts.UpstreamProxy) },
		TLSClientConfig:     tlsConfig(opts.Insecure),
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}

	return &Replayer{
		max: max,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Override 是重放时的可选覆盖项，零值等价于"原样重放"。
//
// 为什么用指针区分"没给"和"给空"：
//   - Body 为 nil 表示沿用源请求体，指向空串表示"确实要清空请求体"；
//   - Headers 为 nil 表示沿用源请求头，非 nil（哪怕是空 map）表示整体替换。
//
// 这不是过度设计：调试接口时"把请求体删掉再发一次"是常见诉求，
// 用空字符串当哨兵会让这种操作静默失效。
type Override struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    *string     `json:"body"`
}

// Do 重放一条会话，返回新的会话详情（ID 已重新生成）。
func (r *Replayer) Do(src *capture.FlowDetail) (*capture.FlowDetail, error) {
	return r.DoWith(src, nil)
}

// DoWith 按覆盖项重放。ov 为 nil 时与原样重放完全一致。
func (r *Replayer) DoWith(src *capture.FlowDetail, ov *Override) (*capture.FlowDetail, error) {
	if src == nil {
		return nil, errors.New("源会话为空")
	}

	target := fullURL(src)
	method := src.Method

	// 只有"要沿用源请求体"时才走 requestBody 的校验；
	// 用户显式提供了新请求体时，源请求体是不是二进制/被截断都不再重要。
	var body []byte
	contentType := src.ReqHeaders.Get("Content-Type")
	if ov == nil || ov.Body == nil {
		var err error
		body, contentType, err = requestBody(src)
		if err != nil {
			return nil, err
		}
	}
	headerSrc := src.ReqHeaders

	if ov != nil {
		if m := strings.ToUpper(strings.TrimSpace(ov.Method)); m != "" {
			if !validMethod(m) {
				return nil, fmt.Errorf("请求方法 %q 不合法", ov.Method)
			}
			method = m
		}
		if u := strings.TrimSpace(ov.URL); u != "" {
			if err := validateTarget(u); err != nil {
				return nil, err
			}
			target = u
		}
		if ov.Headers != nil {
			headerSrc = ov.Headers
			// 覆盖了请求头就按覆盖值决定 Content-Type，避免把源请求的旧值又盖回去
			contentType = ov.Headers.Get("Content-Type")
		}
		if ov.Body != nil {
			body = []byte(*ov.Body)
		}
	}

	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	copyHeaders(req.Header, headerSrc)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// 改了请求体就必须重算长度：否则沿用源请求的 Content-Length，服务端会读少/读多
	if ov != nil && ov.Body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Del("Content-Length")
	}

	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("重放失败: %w", err)
	}
	defer resp.Body.Close()

	raw, truncated, size, err := readLimited(resp.Body, r.max)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	elapsed := time.Since(start)

	resType := resp.Header.Get("Content-Type")
	resBody := capture.BuildBodyView(raw, resType, 0)
	// BuildBodyView 只看得到已经截断的字节，真实大小要由这里补回去
	resBody.Size = size
	resBody.Truncated = truncated

	brief := capture.FlowBrief{
		ID:          NewID("replay"),
		Method:      method,
		Scheme:      schemeOf(target),
		Host:        req.URL.Hostname(),
		Port:        req.URL.Port(),
		Path:        req.URL.EscapedPath(),
		Query:       req.URL.RawQuery,
		StatusCode:  resp.StatusCode,
		ContentType: resType,
		Kind:        capture.ContentKind(resType, req.URL.EscapedPath()),
		ReqSize:     int64(len(body)),
		ResSize:     size,
		DurationMS:  elapsed.Milliseconds(),
		StartTime:   start,
		ServerAddr:  serverAddrOf(src, req.URL),
		ClientAddr:  "本机重放",
		Proto:       resp.Proto,
		Intercepted: true, // 重放是明文读取，不存在"未解密"概念
		Replayed:    true,
	}

	return &capture.FlowDetail{
		FlowBrief:  brief,
		ReqHeaders: req.Header.Clone(),
		ResHeaders: resp.Header.Clone(),
		ReqBody:    capture.BuildBodyView(body, contentType, 0),
		ResBody:    resBody,
	}, nil
}

// serverAddrOf 只在目标 host 未变时沿用源会话的服务端地址。
// 改了 host 还显示旧 IP 会让人误以为请求打到了原服务器，这是最容易踩的坑。
//
// src.Host 正常不含端口（ingest 用 Hostname() 写入），但历史数据/外部构造的会话
// 可能带端口，这里先剥一次再比，避免因为格式差异把"同一台机器"判成不同。
func serverAddrOf(src *capture.FlowDetail, u *url.URL) string {
	host := src.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.EqualFold(host, u.Hostname()) && src.Port == u.Port() {
		return src.ServerAddr
	}
	return ""
}

// validMethod 只做保守校验：RFC 7231 的 token 字符集，避免把换行等注入到请求行。
func validMethod(m string) bool {
	if m == "" || len(m) > 20 {
		return false
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// validateTarget 校验重放目标。只允许 http/https：
// file://、gopher:// 等协议在 http.Client 下行为不可预期，且容易被误用成内网探测工具。
func validateTarget(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("地址格式不正确: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只支持 http/https 地址，当前是 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("地址缺少主机名")
	}
	return nil
}

// requestBody 取出请求体字节。
// 原始请求体是二进制（无法安全还原）时直接拒绝，而不是发送一份被篡改的数据。
func requestBody(src *capture.FlowDetail) (body []byte, contentType string, err error) {
	contentType = src.ReqHeaders.Get("Content-Type")
	if src.ReqBody == nil || src.ReqBody.Kind == "empty" {
		return nil, contentType, nil
	}
	switch src.ReqBody.Kind {
	case "image", "binary":
		return nil, "", errors.New("该请求体是二进制内容，暂不支持重放（可能已被截断，重放会篡改原始数据）")
	}
	if src.ReqBody.Truncated {
		return nil, "", errors.New("该请求体已被截断，重放会发送不完整的数据，已取消")
	}
	// 原始字符集不是 UTF-8 时，重发的是转码后的文本，Content-Type 必须同步声明，
	// 否则服务端会按旧字符集解码出一堆乱码
	if src.ReqBody.Encoding != "" && src.ReqBody.Encoding != "utf-8" && contentType != "" {
		contentType = stripCharset(contentType) + "; charset=utf-8"
	}
	return []byte(src.ReqBody.Preview), contentType, nil
}

// readLimited 读取响应体，返回内容、是否截断与真实大小。
func readLimited(body io.Reader, max int64) (raw []byte, truncated bool, size int64, err error) {
	raw, err = io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, false, 0, err
	}
	size = int64(len(raw))
	if size > max {
		raw = raw[:max]
		truncated = true
		size = max
	}
	return raw, truncated, size, nil
}

// copyHeaders 复制请求头并剔除逐跳字段。
func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func isHopHeader(name string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}

// stripCharset 去掉 Content-Type 里的 charset 参数。
func stripCharset(contentType string) string {
	if i := strings.Index(strings.ToLower(contentType), ";"); i >= 0 {
		return strings.TrimSpace(contentType[:i])
	}
	return strings.TrimSpace(contentType)
}

// parseProxy 解析上游代理地址，空值表示直连。
func parseProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	return url.Parse(raw)
}

// tlsConfig 仅在显式允许时才跳过证书校验；默认沿用系统根证书。
func tlsConfig(insecure bool) *tls.Config {
	if !insecure {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // 由用户显式开启，仅用于调试自签服务
}

func fullURL(f *capture.FlowDetail) string {
	return f.URL()
}

func schemeOf(rawURL string) string {
	if i := strings.Index(rawURL, "://"); i > 0 {
		return rawURL[:i]
	}
	return "http"
}

// NewID 生成会话 ID。前缀便于在日志里一眼看出这是重放产生的会话。
func NewID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf)
}
