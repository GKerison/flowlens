package capture

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func sampleDetail() *FlowDetail {
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)
	return &FlowDetail{
		FlowBrief: FlowBrief{
			Seq:         1,
			ID:          "f1",
			Method:      "POST",
			Scheme:      "https",
			Host:        "api.example.com",
			Port:        "8443",
			Path:        "/v1/login",
			Query:       "a=1&b=%E4%B8%AD",
			StatusCode:  200,
			ContentType: "application/json; charset=utf-8",
			Kind:        "xhr",
			ReqSize:     19,
			ResSize:     21,
			DurationMS:  42,
			StartTime:   start,
			ServerAddr:  "93.184.216.34:443",
			ClientAddr:  "127.0.0.1:51000",
			Proto:       "HTTP/1.1",
			Intercepted: true,
		},
		ReqHeaders: http.Header{
			"Content-Type": []string{"application/json"},
			"Cookie":       []string{"sid=abc; theme=dark"},
		},
		ResHeaders: http.Header{"Content-Type": []string{"application/json"}},
		ReqBody:    BuildBodyView([]byte(`{"user":"张三"}`), "application/json", 1<<20),
		ResBody:    BuildBodyView([]byte(`{"ok":true,"name":"张三"}`), "application/json", 1<<20),
	}
}

func TestBuildHARStructure(t *testing.T) {
	raw, err := BuildHAR([]*FlowDetail{sampleDetail()}, "9.9.9")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	// 必须能被标准 JSON 解析，且字段符合 HAR 1.2 约定
	var parsed struct {
		Log struct {
			Version string `json:"version"`
			Creator struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"creator"`
			Pages   []any `json:"pages"`
			Entries []struct {
				StartedDateTime string  `json:"startedDateTime"`
				Time            float64 `json:"time"`
				Request         struct {
					Method      string `json:"method"`
					URL         string `json:"url"`
					Cookies     []any  `json:"cookies"`
					QueryString []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"queryString"`
					PostData struct {
						Text string `json:"text"`
					} `json:"postData"`
				} `json:"request"`
				Response struct {
					Status  int `json:"status"`
					Content struct {
						Size     int64  `json:"size"`
						MimeType string `json:"mimeType"`
						Text     string `json:"text"`
					} `json:"content"`
				} `json:"response"`
				Timings struct {
					Send    float64 `json:"send"`
					Wait    float64 `json:"wait"`
					Receive float64 `json:"receive"`
				} `json:"timings"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("HAR 不是合法 JSON: %v", err)
	}

	if parsed.Log.Version != "1.2" {
		t.Fatalf("HAR 版本应为 1.2，实际 %s", parsed.Log.Version)
	}
	if parsed.Log.Creator.Name != "FlowLens" || parsed.Log.Creator.Version != "9.9.9" {
		t.Fatalf("creator 信息不正确: %+v", parsed.Log.Creator)
	}
	if parsed.Log.Pages == nil {
		t.Fatal("pages 必须是数组（HAR 规范要求字段存在）")
	}
	if len(parsed.Log.Entries) != 1 {
		t.Fatalf("应有 1 条 entry，实际 %d", len(parsed.Log.Entries))
	}

	e := parsed.Log.Entries[0]
	// 非默认端口必须出现在 URL 里，否则导出的 HAR 无法复现
	if e.Request.URL != "https://api.example.com:8443/v1/login?a=1&b=%E4%B8%AD" {
		t.Fatalf("URL 还原错误: %s", e.Request.URL)
	}
	if len(e.Request.QueryString) != 2 {
		t.Fatalf("query 应有 2 项，实际 %d", len(e.Request.QueryString))
	}
	if e.Request.QueryString[1].Value != "中" {
		t.Fatalf("query 值应做百分号解码，实际 %q", e.Request.QueryString[1].Value)
	}
	if len(e.Request.Cookies) != 2 {
		t.Fatalf("Cookie 应解析出 2 项，实际 %d", len(e.Request.Cookies))
	}
	if e.Request.PostData.Text != `{"user":"张三"}` {
		t.Fatalf("请求体不正确: %q", e.Request.PostData.Text)
	}
	if e.Response.Content.Text != `{"ok":true,"name":"张三"}` {
		t.Fatalf("响应体不正确: %q", e.Response.Content.Text)
	}
	if e.Response.Content.Size != 21 {
		t.Fatalf("content.size 应为原始字节数 21，实际 %d", e.Response.Content.Size)
	}
	if e.Time != 42 {
		t.Fatalf("time 应为 42，实际 %v", e.Time)
	}
}

func TestBuildHARSkipsUnavailableBody(t *testing.T) {
	d := sampleDetail()
	d.Scheme = "https"
	d.Intercepted = false
	d.ResBody = nil
	d.ResHeaders = nil

	raw, err := BuildHAR([]*FlowDetail{d}, "test")
	if err != nil {
		t.Fatal(err)
	}
	var parsed HAR
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Log.Entries[0].Comment == "" {
		t.Fatal("未解密会话应带注释说明")
	}
}

func TestStoreDelete(t *testing.T) {
	s := NewStore(10)
	for _, id := range []string{"a", "b", "c", "d"} {
		s.Add(&FlowDetail{FlowBrief: FlowBrief{ID: id}})
	}
	s.AppendFrame("b", NewFrame(0, true, false, []byte("hi"), time.Now()))

	deleted := s.Delete([]string{"b", "c", "missing"})
	if len(deleted) != 2 {
		t.Fatalf("应删除 2 条，实际 %v", deleted)
	}
	if s.Len() != 2 {
		t.Fatalf("剩余应为 2 条，实际 %d", s.Len())
	}
	if list := s.ListSince(0); len(list) != 2 || list[0].ID != "a" || list[1].ID != "d" {
		t.Fatalf("删除后顺序被打乱: %+v", list)
	}
	if frames, _ := s.Frames("b"); len(frames) != 0 {
		t.Fatal("被删会话的帧应一并清理")
	}
}

func TestStoreFramesLimit(t *testing.T) {
	s := NewStore(10)
	s.Add(&FlowDetail{FlowBrief: FlowBrief{ID: "ws1"}})

	for i := 0; i < maxFramesPerFlow+5; i++ {
		s.AppendFrame("ws1", NewFrame(i, i%2 == 0, false, []byte("x"), time.Now()))
	}
	frames, dropped := s.Frames("ws1")
	if len(frames) != maxFramesPerFlow {
		t.Fatalf("帧数应被限制在 %d，实际 %d", maxFramesPerFlow, len(frames))
	}
	if dropped != 5 {
		t.Fatalf("被丢弃计数应为 5，实际 %d", dropped)
	}
	if d, _ := s.Get("ws1"); d.Frames != maxFramesPerFlow {
		t.Fatalf("会话上的帧计数应同步，实际 %d", d.Frames)
	}
}

func TestFrameBinaryAndText(t *testing.T) {
	text := NewFrame(0, true, false, []byte("你好"), time.Now())
	if text.Kind != "text" || text.Text != "你好" {
		t.Fatalf("文本帧解析错误: %+v", text)
	}
	bin := NewFrame(1, false, true, []byte{0x00, 0x01, 0xFF}, time.Now())
	if bin.Kind != "binary" || bin.Base64 == "" {
		t.Fatalf("二进制帧应内联 base64: %+v", bin)
	}
	big := NewFrame(2, false, true, make([]byte, maxFrameBinary+1), time.Now())
	if !big.Truncated || big.Base64 != "" {
		t.Fatal("超大二进制帧不应内联内容")
	}
}

func TestFlowBriefURLPort(t *testing.T) {
	cases := []struct {
		name string
		b    FlowBrief
		want string
	}{
		{"默认端口省略", FlowBrief{Scheme: "http", Host: "a.com", Port: "80", Path: "/x"}, "http://a.com/x"},
		{"https 默认端口省略", FlowBrief{Scheme: "https", Host: "a.com", Port: "443", Path: "/x"}, "https://a.com/x"},
		{"非默认端口保留", FlowBrief{Scheme: "http", Host: "127.0.0.1", Port: "8890", Path: "/gbk"}, "http://127.0.0.1:8890/gbk"},
		{"无端口", FlowBrief{Scheme: "https", Host: "a.com", Path: "/"}, "https://a.com/"},
		{"空路径补斜杠", FlowBrief{Scheme: "http", Host: "a.com", Query: "k=v"}, "http://a.com/?k=v"},
	}
	for _, c := range cases {
		if got := c.b.URL(); got != c.want {
			t.Errorf("%s: 期望 %s，实际 %s", c.name, c.want, got)
		}
	}
}
