// Package capture 定义会话数据模型与内存存储。
//
// 分层原则：本包只负责"数据长什么样、怎么存"，不关心代理内核如何产生数据，
// 也不关心界面如何展示。内核适配在 internal/proxy，界面绑定在 internal/service。
package capture

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
)

// FlowBrief 是列表页所需的轻量会话视图，**不含 body**，确保高频推送时体积可控。
type FlowBrief struct {
	Seq         uint64    `json:"seq"` // 单调递增，供前端增量拉取
	ID          string    `json:"id"`
	Method      string    `json:"method"`
	Scheme      string    `json:"scheme"`
	Host        string    `json:"host"`
	Port        string    `json:"port,omitempty"` // 非默认端口时才有值，用于还原完整 URL
	Path        string    `json:"path"`
	Query       string    `json:"query"`
	StatusCode  int       `json:"statusCode"`
	ContentType string    `json:"contentType"`
	Kind        string    `json:"kind"` // xhr/js/css/img/font/media/ws/doc/other，供前端按类型过滤
	ReqSize     int64     `json:"reqSize"`
	ResSize     int64     `json:"resSize"`
	DurationMS  int64     `json:"durationMs"`
	StartTime   time.Time `json:"startTime"`
	ClientAddr  string    `json:"clientAddr"`
	ServerAddr  string    `json:"serverAddr"`
	Proto       string    `json:"proto"`
	Intercepted bool      `json:"intercepted"`      // false 表示仅 CONNECT 隧道，未解密
	Frames      int       `json:"frames"`           // WebSocket/SSE 已捕获的消息数
	Replayed    bool      `json:"replayed"`         // true 表示由"请求重放"产生
	Mapped      string    `json:"mapped,omitempty"` // 命中的 Map Local 规则（本地文件名），非空表示响应被替换
	Remoted     string    `json:"remoted,omitempty"` // 命中的 Map Remote 目标，非空表示请求被改写
	Error       string    `json:"error,omitempty"`  // 请求失败原因
}

// BodyView 是 body 的展示模型。二进制内容不直接塞进 JSON，改由前端按需取用。
type BodyView struct {
	Kind      string `json:"kind"` // json | html | xml | text | image | binary | empty
	MIME      string `json:"mime"` // 原始 Content-Type
	Size      int64  `json:"size"` // 解压后的真实大小
	Encoding  string `json:"encoding,omitempty"`
	Truncated bool   `json:"truncated"`
	Preview   string `json:"preview,omitempty"` // 文本类内容（超限时已截断）
	Base64    string `json:"base64,omitempty"`  // 图片内容，仅小图内联
}

// FlowDetail 是详情页完整数据，按需拉取。
type FlowDetail struct {
	FlowBrief
	ReqHeaders http.Header `json:"reqHeaders"`
	ResHeaders http.Header `json:"resHeaders"`
	ReqBody    *BodyView   `json:"reqBody,omitempty"`
	ResBody    *BodyView   `json:"resBody,omitempty"`
}

// maxInlineImage 是内联到 JSON 的图片上限。超过则只标记类型，不传二进制，
// 避免单条详情消息几十 MB 直接卡死 WebView。
const maxInlineImage = 512 << 10

// BuildBodyView 将原始字节与 Content-Type 转换为展示模型。
// limit 为保留上限：超过部分丢弃并标记 Truncated，防止内存失控。
//
// 文本类内容先做字符集探测再分类（见 charset.go）：
// 直接把 GBK 字节按 UTF-8 渲染会让整段中文变成乱码方块，这是抓包工具最常见的体验缺陷。
func BuildBodyView(raw []byte, contentType string, limit int64) *BodyView {
	if len(raw) == 0 {
		return &BodyView{Kind: "empty", MIME: contentType, Size: 0}
	}

	size := int64(len(raw))
	if limit > 0 && size > limit {
		raw = raw[:limit]
	}

	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	v := &BodyView{
		MIME:      contentType,
		Size:      size,
		Truncated: size > int64(len(raw)),
	}

	// 图片走二进制通道内联展示，不参与文本探测（图片字节常恰好"看起来像文本"）
	if isImage(mime) {
		v.Kind = "image"
		if len(raw) <= maxInlineImage {
			v.Base64 = base64.StdEncoding.EncodeToString(raw)
		}
		return v
	}

	if !isTextualMIME(mime) && !looksLikeText(raw) {
		v.Kind = "binary"
		return v
	}

	text, enc := DecodeText(raw, contentType)
	v.Encoding = enc
	v.Preview = text
	v.Kind = classifyText(mime, text)
	return v
}

// classifyText 在已解码的文本上判断具体展示类型。
// 优先信任 Content-Type，缺失或为万能类型时再用内容特征兜底。
func classifyText(mime, text string) string {
	switch {
	case isJSON(mime) || strings.Contains(mime, "x-ndjson"):
		return "json"
	case strings.Contains(mime, "html"):
		return "html"
	case strings.Contains(mime, "xml"):
		return "xml"
	}
	if isText(mime) || strings.Contains(mime, "graphql") {
		return "text"
	}
	// 万能类型（application/octet-stream 等）按内容猜
	trimmed := strings.TrimSpace(text)
	switch {
	case looksLikeJSON(trimmed):
		return "json"
	case strings.HasPrefix(strings.ToLower(trimmed), "<!doctype html"),
		strings.HasPrefix(strings.ToLower(trimmed), "<html"):
		return "html"
	case strings.HasPrefix(trimmed, "<?xml"):
		return "xml"
	default:
		return "text"
	}
}

// DefaultPort 返回该 scheme 的默认端口，未知 scheme 返回空串。
func DefaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https", "wss":
		return "443"
	case "http", "ws":
		return "80"
	}
	return ""
}

// URL 还原完整请求地址。
// 端口非默认时必须带上，否则 HAR 导出、cURL 复制、请求重放都会指向错误地址。
func (b FlowBrief) URL() string {
	scheme := b.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host := b.Host
	if b.Port != "" && b.Port != DefaultPort(scheme) {
		host = net.JoinHostPort(b.Host, b.Port)
	}
	path := b.Path
	if path == "" {
		path = "/"
	}
	u := scheme + "://" + host + path
	if b.Query != "" {
		u += "?" + b.Query
	}
	return u
}

// ContentKind 根据 Content-Type 与路径推断资源类型，用于列表图标与过滤。
func ContentKind(contentType, path string) string {
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch {
	case strings.Contains(mime, "json") || strings.Contains(mime, "graphql"):
		return "xhr"
	case strings.Contains(mime, "javascript") || strings.HasSuffix(path, ".js"):
		return "js"
	case strings.Contains(mime, "css") || strings.HasSuffix(path, ".css"):
		return "css"
	case isImage(mime):
		return "img"
	case strings.Contains(mime, "font") || strings.HasSuffix(path, ".woff2") || strings.HasSuffix(path, ".ttf"):
		return "font"
	case strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/"):
		return "media"
	case strings.Contains(mime, "html"):
		return "doc"
	case strings.Contains(mime, "event-stream"):
		return "sse"
	default:
		return "other"
	}
}
func isJSON(mime string) bool {
	return strings.Contains(mime, "json")
}

func isImage(mime string) bool {
	return strings.HasPrefix(mime, "image/") && !strings.Contains(mime, "svg")
}

func isText(mime string) bool {
	return strings.HasPrefix(mime, "text/") ||
		strings.Contains(mime, "xml") ||
		strings.Contains(mime, "javascript") ||
		strings.Contains(mime, "x-www-form-urlencoded")
}

// looksLikeJSON 在缺少 Content-Type 时用启发式判断，避免把 JSON 当纯文本展示。
// 入参是已解码的文本，此处只做语法校验。
func looksLikeJSON(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return false
	}
	return json.Valid([]byte(trimmed))
}
