package service_test

import (
	"testing"

	"github.com/kerison/flowlens/internal/capture"
)

// 响应体搜索的取舍：
//  - 搜的是解码后的 Preview（与详情页所见一致），GBK 等编码的中文也能命中；
//  - 大小写不敏感；空关键字返回空而不是"全部"，避免误把过滤当成全量拉取。

func bodyFlow(id, host, text string) *capture.FlowDetail {
	d := &capture.FlowDetail{}
	d.ID = id
	d.Host = host
	d.ResBody = &capture.BodyView{Kind: "json", Preview: text}
	return d
}

func TestSearchBodiesMatchesDecodedPreview(t *testing.T) {
	app, _, store := newTestAppWithStore(t)
	store.Add(bodyFlow("f1", "api.a.com", `{"msg":"登录成功"}`))
	store.Add(bodyFlow("f2", "api.b.com", `{"error":"invalid token"}`))
	store.Add(bodyFlow("f3", "api.c.com", "plain text without keyword"))

	got := app.SearchBodies("登录")
	if len(got) != 1 || got[0].ID != "f1" {
		t.Fatalf("中文关键字应命中 f1，实际 %+v", got)
	}

	got = app.SearchBodies("INVALID")
	if len(got) != 1 || got[0].ID != "f2" {
		t.Fatalf("关键字应大小写不敏感地命中 f2，实际 %+v", got)
	}

	if got := app.SearchBodies("不存在的关键字"); len(got) != 0 {
		t.Fatalf("未命中应返回空，实际 %+v", got)
	}
}

func TestSearchBodiesEmptyKeywordReturnsEmpty(t *testing.T) {
	app, _, store := newTestAppWithStore(t)
	store.Add(bodyFlow("f1", "api.a.com", "hello"))

	if got := app.SearchBodies("   "); len(got) != 0 {
		t.Fatalf("空白关键字不应返回任何结果，实际 %+v", got)
	}
}
