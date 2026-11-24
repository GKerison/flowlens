package capture

import (
	"fmt"
	"testing"
	"time"
)

// 这组基准用来回答"M5 的性能压测"里唯一真正的问题：
// 会话数到达容量上限后，每次新增的代价是否还保持在常数级别。
// 跑法：go test ./internal/capture -run '^$' -bench . -benchmem

func benchFlow(i int) *FlowDetail {
	return &FlowDetail{
		FlowBrief: FlowBrief{
			ID:          fmt.Sprintf("flow-%08d", i),
			Method:      "GET",
			Scheme:      "https",
			Host:        "api.example.com",
			Path:        fmt.Sprintf("/v1/items/%d", i),
			StatusCode:  200,
			ContentType: "application/json",
			Kind:        "xhr",
			ResSize:     1873,
			DurationMS:  42,
			StartTime:   time.Now(),
			Intercepted: true,
		},
	}
}

// BenchmarkStoreAddAtCapacity 压测"满载后持续写入"，也就是真实抓包的稳态。
func BenchmarkStoreAddAtCapacity(b *testing.B) {
	s := NewStore(10000)
	for i := 0; i < 10000; i++ {
		s.Add(benchFlow(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Add(benchFlow(10000 + i))
	}
}

// BenchmarkStoreListSinceTail 覆盖前端轮询最常见的路径：只取最后几条增量。
func BenchmarkStoreListSinceTail(b *testing.B) {
	s := NewStore(10000)
	for i := 0; i < 10000; i++ {
		s.Add(benchFlow(i))
	}
	since := uint64(9990)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := s.ListSince(since); len(got) != 10 {
			b.Fatalf("增量条数应为 10，实际 %d", len(got))
		}
	}
}

// BenchmarkStoreListSinceFull 覆盖首屏首次全量拉取。
func BenchmarkStoreListSinceFull(b *testing.B) {
	s := NewStore(10000)
	for i := 0; i < 10000; i++ {
		s.Add(benchFlow(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := s.ListSince(0); len(got) != 10000 {
			b.Fatalf("全量条数应为 10000，实际 %d", len(got))
		}
	}
}

// BenchmarkStoreGet 覆盖点开详情：应为 O(1)。
func BenchmarkStoreGet(b *testing.B) {
	s := NewStore(10000)
	for i := 0; i < 10000; i++ {
		s.Add(benchFlow(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := s.Get("flow-00005000"); !ok {
			b.Fatal("会话应存在")
		}
	}
}
