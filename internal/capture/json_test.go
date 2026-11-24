package capture

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestBuildJSONStructure(t *testing.T) {
	f := sampleDetail()
	f.Mapped = "/Users/me/mock/login.json"
	f.Frames = 3

	raw, err := BuildJSON([]*FlowDetail{f, nil}, "9.9.9")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	var got JSONExport
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("导出的 JSON 不能解析: %v", err)
	}
	if got.Format != JSONFormat {
		t.Fatalf("格式标识应为 %s，实际 %s", JSONFormat, got.Format)
	}
	if got.Creator.Name != "FlowLens" || got.Creator.Version != "9.9.9" {
		t.Fatalf("creator 不正确: %+v", got.Creator)
	}
	// nil 会话必须被跳过，且 count 要与实际条数一致（下游脚本依赖它做校验）
	if got.Count != 1 || len(got.Flows) != 1 {
		t.Fatalf("应导出 1 条，实际 count=%d flows=%d", got.Count, len(got.Flows))
	}

	flow := got.Flows[0]
	// 非默认端口必须出现在 URL 里，否则导出的地址指向错误位置
	if flow.URL != "https://api.example.com:8443/v1/login?a=1&b=%E4%B8%AD" {
		t.Fatalf("URL 还原不正确: %s", flow.URL)
	}
	if flow.Port != "8443" || flow.Method != "POST" || flow.StatusCode != 200 {
		t.Fatalf("基础字段不正确: %+v", flow)
	}
	if flow.Mapped == "" || flow.Frames != 3 {
		t.Fatalf("Map Local / 帧数应保留: mapped=%q frames=%d", flow.Mapped, flow.Frames)
	}
	if flow.StartTime == "" {
		t.Fatal("startTime 不能为空")
	}
}

func TestBuildJSONHeadersKeepOrderAndDuplicates(t *testing.T) {
	f := sampleDetail()
	// 同名多值在 map→数组的转换里最容易丢，这里显式验证
	f.ResHeaders = http.Header{
		"Set-Cookie": []string{"a=1", "b=2"},
		"X-Trace":    []string{"t1"},
	}

	raw, err := BuildJSON([]*FlowDetail{f}, "v")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	var got JSONExport
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var cookies []string
	for _, h := range got.Flows[0].Response.Headers {
		if h.Name == "Set-Cookie" {
			cookies = append(cookies, h.Value)
		}
	}
	if len(cookies) != 2 || cookies[0] != "a=1" || cookies[1] != "b=2" {
		t.Fatalf("Set-Cookie 多值与顺序应保留，实际 %v", cookies)
	}
}

func TestBuildJSONKeepsBody(t *testing.T) {
	f := sampleDetail()
	raw, err := BuildJSON([]*FlowDetail{f}, "v")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	var got JSONExport
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Flows[0].Request.Body == nil || got.Flows[0].Request.Body.Preview == "" {
		t.Fatal("请求体应被保留")
	}
	if got.Flows[0].Request.Body.Kind != "json" {
		t.Fatalf("请求体类型应为 json，实际 %s", got.Flows[0].Request.Body.Kind)
	}
}

func TestBuildJSONEmpty(t *testing.T) {
	raw, err := BuildJSON(nil, "v")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	var got JSONExport
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// flows 应为 [] 而不是 null：null 会让多数脚本直接报错
	if got.Flows == nil || got.Count != 0 {
		t.Fatalf("空导出应得到空数组，实际 %#v", got.Flows)
	}
}
