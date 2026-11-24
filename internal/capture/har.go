package capture

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// 本文件实现 HAR 1.2 导出（http://www.softwareishard.com/blog/har-12-spec/）。
//
// 之所以自己组装而不引第三方库：HAR 结构固定且字段少，自建可精确控制
// 截断标记、未解密标记等 FlowLens 特有信息，避免为几十行结构引入一个依赖。

// HAR 是导出文件的根结构。
type HAR struct {
	Log HARLog `json:"log"`
}

// HARLog 是 HAR 的主体。
type HARLog struct {
	Version string     `json:"version"`
	Creator HARCreator `json:"creator"`
	Pages   []any      `json:"pages"`
	Entries []HAREntry `json:"entries"`
	Comment string     `json:"comment,omitempty"`
}

// HARCreator 记录导出工具信息。
type HARCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// HAREntry 对应一次请求/响应。
type HAREntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         HARRequest  `json:"request"`
	Response        HARResponse `json:"response"`
	Cache           struct{}    `json:"cache"`
	Timings         HARTimings  `json:"timings"`
	ServerIPAddress string      `json:"serverIPAddress,omitempty"`
	Connection      string      `json:"connection,omitempty"`
	Comment         string      `json:"comment,omitempty"`
}

// HARRequest 是请求部分。
type HARRequest struct {
	Method      string       `json:"method"`
	URL         string       `json:"url"`
	HTTPVersion string       `json:"httpVersion"`
	Cookies     []HARCookie  `json:"cookies"`
	Headers     []HARNameVal `json:"headers"`
	QueryString []HARNameVal `json:"queryString"`
	PostData    *HARPostData `json:"postData,omitempty"`
	HeadersSize int64        `json:"headersSize"`
	BodySize    int64        `json:"bodySize"`
}

// HARResponse 是响应部分。
type HARResponse struct {
	Status      int          `json:"status"`
	StatusText  string       `json:"statusText"`
	HTTPVersion string       `json:"httpVersion"`
	Cookies     []HARCookie  `json:"cookies"`
	Headers     []HARNameVal `json:"headers"`
	Content     HARContent   `json:"content"`
	RedirectURL string       `json:"redirectURL"`
	HeadersSize int64        `json:"headersSize"`
	BodySize    int64        `json:"bodySize"`
}

// HARContent 是响应正文。
type HARContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Encoding string `json:"encoding,omitempty"` // 仅 "base64" 有效
	Comment  string `json:"comment,omitempty"`
}

// HARPostData 是请求正文。
type HARPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Comment  string `json:"comment,omitempty"`
}

// HARTimings 是分阶段耗时。本项目只测总耗时，故只填 wait。
type HARTimings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// HARCookie 是 cookie 项。HAR 要求该字段存在，即使为空也应为数组。
type HARCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HARNameVal 是通用的 name/value 对（headers、queryString 共用）。
type HARNameVal struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// BuildHAR 把会话转换为 HAR 1.2 JSON。
// version 用于填充 creator.version，便于以后排查是哪一版应用导出的文件。
func BuildHAR(flows []*FlowDetail, version string) ([]byte, error) {
	log := HARLog{
		Version: "1.2",
		Creator: HARCreator{Name: "FlowLens", Version: version},
		Pages:   []any{},
		Entries: make([]HAREntry, 0, len(flows)),
	}
	for _, f := range flows {
		if f == nil {
			continue
		}
		log.Entries = append(log.Entries, buildEntry(f))
	}
	return json.MarshalIndent(HAR{Log: log}, "", "  ")
}

func buildEntry(f *FlowDetail) HAREntry {
	url := fullURL(f)
	entry := HAREntry{
		StartedDateTime: f.StartTime.Format("2006-01-02T15:04:05.000Z07:00"),
		Time:            float64(f.DurationMS),
		Request: HARRequest{
			Method:      f.Method,
			URL:         url,
			HTTPVersion: harHTTPVersion(f.Proto),
			Cookies:     parseCookies(f.ReqHeaders),
			Headers:     toNameVals(f.ReqHeaders),
			QueryString: parseQuery(f.Query),
			HeadersSize: -1, // 未保留原始头部字节数，按规范填 -1 表示未知
			BodySize:    f.ReqSize,
		},
		Response: HARResponse{
			Status:      f.StatusCode,
			StatusText:  http.StatusText(f.StatusCode),
			HTTPVersion: harHTTPVersion(f.Proto),
			Cookies:     parseCookies(f.ResHeaders),
			Headers:     toNameVals(f.ResHeaders),
			HeadersSize: -1,
			BodySize:    f.ResSize,
		},
		Timings:         HARTimings{Send: 0, Wait: float64(f.DurationMS), Receive: 0},
		ServerIPAddress: hostOnly(f.ServerAddr),
		Connection:      f.ClientAddr,
	}

	if f.ReqBody != nil && f.ReqBody.Kind != "empty" {
		if pd := harPostData(f.ReqBody); pd != nil {
			entry.Request.PostData = pd
		}
	}

	entry.Response.Content = HARContent{
		Size:     f.ResSize,
		MimeType: f.ContentType,
	}
	if f.ResBody != nil {
		switch f.ResBody.Kind {
		case "image":
			if f.ResBody.Base64 != "" {
				entry.Response.Content.Text = f.ResBody.Base64
				entry.Response.Content.Encoding = "base64"
			} else {
				entry.Response.Content.Comment = "图片过大，未内联"
			}
		case "binary":
			entry.Response.Content.Comment = "二进制内容，未内联"
		case "empty":
			// 无内容，留空
		default:
			entry.Response.Content.Text = f.ResBody.Preview
			if f.ResBody.Encoding != "" && f.ResBody.Encoding != "utf-8" {
				entry.Response.Content.Comment = "原始字符集 " + f.ResBody.Encoding + "，已转 UTF-8"
			}
		}
		if f.ResBody.Truncated {
			entry.Response.Content.Comment = joinComment(entry.Response.Content.Comment, "响应体已按上限截断，内容不完整")
		}
	}

	// 未解密的 HTTPS 隧道只有 CONNECT 元信息，导出时说明清楚，避免别人误读为"抓不到 body 是工具坏了"
	if f.Scheme == "https" && !f.Intercepted {
		entry.Comment = "该请求仅建立 CONNECT 隧道，未解密，无内容详情"
	}
	if f.Error != "" {
		entry.Comment = joinComment(entry.Comment, "请求失败："+f.Error)
	}
	return entry
}

// harPostData 组装请求体。二进制请求体不内联，只记录大小。
func harPostData(b *BodyView) *HARPostData {
	pd := &HARPostData{MimeType: b.MIME}
	switch b.Kind {
	case "image", "binary":
		pd.Text = ""
		pd.Comment = "二进制请求体，未内联"
		return pd
	default:
		pd.Text = b.Preview
	}
	if b.Encoding != "" && b.Encoding != "utf-8" {
		pd.Comment = joinComment(pd.Comment, "原始字符集 "+b.Encoding+"，已转 UTF-8")
	}
	if b.Truncated {
		pd.Comment = joinComment(pd.Comment, "请求体已按上限截断")
	}
	return pd
}

// fullURL 还原完整请求地址。Path 已转义，Query 保持原始顺序。
func fullURL(f *FlowDetail) string {
	return f.URL()
}

// toNameVals 把 Header 展开成 HAR 的 name/value 数组，按名称排序保证导出可复现。
func toNameVals(h http.Header) []HARNameVal {
	out := make([]HARNameVal, 0, len(h))
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			out = append(out, HARNameVal{Name: k, Value: v})
		}
	}
	return out
}

// parseQuery 按原始顺序解析 query string（url.Values 会按 key 排序，丢失原始顺序）。
func parseQuery(raw string) []HARNameVal {
	out := []HARNameVal{}
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		out = append(out, HARNameVal{Name: unescapeQuery(k), Value: unescapeQuery(v)})
	}
	return out
}

// parseCookies 从 Cookie 请求头提取 cookie 列表。
func parseCookies(h http.Header) []HARCookie {
	out := []HARCookie{}
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			k, v, _ := strings.Cut(part, "=")
			out = append(out, HARCookie{Name: k, Value: v})
		}
	}
	return out
}

// harHTTPVersion 把 Go 的 Proto 转成 HAR 惯用的写法。
func harHTTPVersion(proto string) string {
	if proto == "" {
		return "HTTP/1.1"
	}
	return proto
}

// hostOnly 从 "1.2.3.4:443" 里取出 IP，HAR 的 serverIPAddress 不带端口。
func hostOnly(addr string) string {
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// joinComment 拼接注释，已有内容时用分号分隔。
func joinComment(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "；" + add
}

// unescapeQuery 做最小化的百分号解码，失败时原样返回（宁可展示原始值也不丢信息）。
func unescapeQuery(s string) string {
	if !strings.Contains(s, "%") && !strings.Contains(s, "+") {
		return s
	}
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}
