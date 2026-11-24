package capture

import (
	"fmt"
	"testing"
)

// 存储用"移动窗口 + 定期压缩"实现淘汰，窗口边界很容易写错，
// 这里把每个受影响的入口都覆盖一遍。

// idsOf 取出会话 ID 序列，失败信息里直接看得到顺序。
func idsOf(briefs []FlowBrief) []string {
	out := make([]string, len(briefs))
	for i, b := range briefs {
		out[i] = b.ID
	}
	return out
}

func fill(s *Store, from, to int) {
	for i := from; i < to; i++ {
		s.Add(benchFlow(i))
	}
}

func TestStoreEvictsOldest(t *testing.T) {
	s := NewStore(3)
	fill(s, 0, 5)

	if got := s.Len(); got != 3 {
		t.Fatalf("应保留 3 条，实际 %d", got)
	}
	got := idsOf(s.ListSince(0))
	want := []string{"flow-00000002", "flow-00000003", "flow-00000004"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("保留顺序不正确：%v", got)
	}
	for i := 0; i < 2; i++ {
		if _, ok := s.Get(fmt.Sprintf("flow-%08d", i)); ok {
			t.Fatalf("第 %d 条应已被淘汰", i)
		}
	}
}

func TestStoreListSinceAfterEviction(t *testing.T) {
	s := NewStore(10)
	fill(s, 0, 100)

	all := s.ListSince(0)
	if len(all) != 10 {
		t.Fatalf("全量应为 10 条，实际 %d", len(all))
	}
	// seq 必须严格递增且连续（淘汰不改变已有 seq）
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("seq 未递增：%d -> %d", all[i-1].Seq, all[i].Seq)
		}
	}

	last := all[len(all)-1]
	inc := s.ListSince(last.Seq - 3)
	if len(inc) != 3 {
		t.Fatalf("增量应为 3 条，实际 %d", len(inc))
	}
	if inc[len(inc)-1].Seq != last.Seq {
		t.Fatalf("增量最后一条应是 seq=%d，实际 %d", last.Seq, inc[len(inc)-1].Seq)
	}
}

// TestStoreWindowStaysBounded 保证长期运行不会因为底层数组只增不减吃内存。
func TestStoreWindowStaysBounded(t *testing.T) {
	const max = 100
	s := NewStore(max)
	fill(s, 0, 10000)

	if got := s.Len(); got != max {
		t.Fatalf("应保留 %d 条，实际 %d", max, got)
	}
	if cap(s.order) > max*4 {
		t.Fatalf("底层数组增长失控：cap=%d（上限 %d）", cap(s.order), max)
	}
	if s.head >= len(s.order) {
		t.Fatalf("窗口越界：head=%d len=%d", s.head, len(s.order))
	}
}

func TestStoreDeleteAfterEviction(t *testing.T) {
	s := NewStore(5)
	fill(s, 0, 8)

	// 删中间一条，剩下的顺序不能乱
	deleted := s.Delete([]string{"flow-00000005"})
	if len(deleted) != 1 {
		t.Fatalf("应删除 1 条，实际 %v", deleted)
	}
	got := idsOf(s.ListSince(0))
	want := []string{"flow-00000003", "flow-00000004", "flow-00000006", "flow-00000007"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("删除后顺序不正确：%v", got)
	}
	if _, ok := s.Get("flow-00000005"); ok {
		t.Fatal("被删除的会话不应还能取到")
	}
}

func TestStoreResizeShrinks(t *testing.T) {
	s := NewStore(100)
	fill(s, 0, 100)

	s.Resize(10)
	if got := s.Len(); got != 10 {
		t.Fatalf("缩容后应保留 10 条，实际 %d", got)
	}
	all := s.ListSince(0)
	if all[0].ID != "flow-00000090" {
		t.Fatalf("缩容应淘汰最旧的，实际首条 %s", all[0].ID)
	}
}

// TestStoreDuplicateAddKeepsSeq 锁定"响应回填不改变顺序"这条约定。
func TestStoreDuplicateAddKeepsSeq(t *testing.T) {
	s := NewStore(10)
	fill(s, 0, 3)
	before := s.ListSince(0)[1]

	updated := benchFlow(1)
	updated.StatusCode = 500
	if _, isNew := s.Add(updated); isNew {
		t.Fatal("同 ID 重复写入不应算新增")
	}

	after := s.ListSince(0)
	if len(after) != 3 {
		t.Fatalf("重复写入不应增加条数，实际 %d", len(after))
	}
	if after[1].Seq != before.Seq {
		t.Fatalf("seq 被改写：%d -> %d", before.Seq, after[1].Seq)
	}
	if after[1].StatusCode != 500 {
		t.Fatalf("内容未被更新：%+v", after[1])
	}
}

// TestStoreFramesRejectEvicted 保证淘汰后残留帧不会变成无主内存。
func TestStoreFramesRejectEvicted(t *testing.T) {
	s := NewStore(1)
	s.Add(benchFlow(0))
	if kept, _, _ := s.AppendFrame("flow-00000000", Frame{Size: 10}); !kept {
		t.Fatal("存活会话应接收帧")
	}

	s.Add(benchFlow(1)) // 淘汰第一条
	kept, total, _ := s.AppendFrame("flow-00000000", Frame{Size: 10})
	if kept || total != 0 {
		t.Fatalf("已淘汰会话不应接收帧：kept=%v total=%d", kept, total)
	}
	if frames, _ := s.Frames("flow-00000000"); len(frames) != 0 {
		t.Fatalf("已淘汰会话的帧应一并清空，实际 %d 帧", len(frames))
	}
}
