package capture

import (
	"sync"
)

const (
	// maxFramesPerFlow 是单条长连接保留的帧数上限。
	// 2000 帧足以覆盖绝大多数调试场景，同时把内存控制在可控范围。
	maxFramesPerFlow = 2000
	// maxFrameBytesPerFlow 是单条长连接帧内容的字节上限。
	maxFrameBytesPerFlow = 8 << 20
)

// Store 是会话的内存存储：写入频繁、读取频繁、容量固定。
//
// 结构选择：用切片维护插入顺序 + map 做 O(1) 查找，超出上限时淘汰最旧记录。
// 之所以不用环形缓冲，是因为前端需要"按 seq 增量拉取"，
// 切片天然有序，从尾部倒查即可，逻辑更简单且没有取模开销。
type Store struct {
	mu  sync.RWMutex
	max int
	seq uint64
	// order 按插入顺序保存 ID；head 之前的元素已被淘汰。
	// 用"移动窗口"而不是每次淘汰都重建切片：满载稳态下每次新增都是 O(1)，
	// 重建切片的代价是 O(max)，一万条会话时单次新增要拷贝上万个字符串。
	order []string
	head  int
	items map[string]*FlowDetail // ID -> 详情
	ws    map[string]*frameSet   // ID -> 该会话的 WebSocket/SSE 帧
}

// frameSet 是一条会话的帧集合，自带字节计量用于限流。
type frameSet struct {
	items   []Frame
	bytes   int
	dropped int // 因超出上限被丢弃的帧数，用于在界面上如实告知用户
}

// NewStore 创建存储，max 为保留上限（<=0 时使用 5000）。
func NewStore(max int) *Store {
	if max <= 0 {
		max = 5000
	}
	return &Store{
		max:   max,
		items: make(map[string]*FlowDetail, max),
		order: make([]string, 0, max),
		ws:    make(map[string]*frameSet),
	}
}

// Add 写入一条会话，返回轻量视图与"是否新增"。
// 同一 ID 重复写入（如请求失败后补上响应）时更新内容但保留原 seq，
// 这样前端按 seq 增量拉取的顺序不会被破坏；内容变化由前端按 id 覆盖。
func (s *Store) Add(d *FlowDetail) (FlowBrief, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old, exists := s.items[d.ID]; exists {
		d.Seq = old.Seq
		// 帧数由帧存储维护，重新入库时不能把已累计的数量覆盖成 0
		if fs := s.ws[d.ID]; fs != nil {
			d.Frames = len(fs.items)
		}
		s.items[d.ID] = d
		return d.FlowBrief, false
	}

	s.seq++
	d.Seq = s.seq
	s.items[d.ID] = d
	s.order = append(s.order, d.ID)

	// 超出上限：从窗口头部淘汰
	if live := len(s.order) - s.head; live > s.max {
		s.evictLocked(live - s.max)
	}

	return d.FlowBrief, true
}

// evictLocked 淘汰最旧的 n 条会话。调用方必须持有写锁。
func (s *Store) evictLocked(n int) {
	if n <= 0 {
		return
	}
	end := s.head + n
	if end > len(s.order) {
		end = len(s.order)
	}
	for _, id := range s.order[s.head:end] {
		delete(s.items, id)
		delete(s.ws, id)
	}
	s.head = end

	// 整理时机：死元素攒到一定量才搬，摊薄后仍是 O(1)。
	// 不整理会让底层数组一直增长（dead + live 都占内存），整理太勤又退化成每次拷贝。
	if s.head >= 64 && s.head*2 >= len(s.order) {
		s.compactLocked()
	}
}

// compactLocked 把窗口压回数组头部，并把腾出的槽位清空让 GC 能回收。
func (s *Store) compactLocked() {
	if s.head == 0 {
		return
	}
	n := copy(s.order, s.order[s.head:])
	for i := n; i < len(s.order); i++ {
		s.order[i] = ""
	}
	s.order = s.order[:n]
	s.head = 0
}

// ListSince 返回 seq 大于 since 的会话（按时间正序）。
// 前端首次加载传 0 拿全量，之后传本地最大 seq 拿增量。
func (s *Store) ListSince(since uint64) []FlowBrief {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 容量取小一点：增量轮询通常只有几条，按 64 预分配等于每次都多分配十几 KB
	out := make([]FlowBrief, 0, 16)
	for i := len(s.order) - 1; i >= s.head; i-- {
		item, ok := s.items[s.order[i]]
		if !ok {
			continue
		}
		if item.Seq <= since {
			break // order 按 seq 递增，遇到不大于 since 的即可停止
		}
		out = append(out, item.FlowBrief)
	}
	// 上面是倒序收集的，反转回正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Get 按 ID 取详情。返回值只读，调用方不得修改（并发安全靠"只读约定"保证）。
func (s *Store) Get(id string) (*FlowDetail, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.items[id]
	return d, ok
}

// GetAll 返回全部会话详情（按时间正序），供 HAR 导出等整批操作使用。
// 返回的是指针切片，调用方仍须按只读对待。
func (s *Store) GetAll() []*FlowDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*FlowDetail, 0, len(s.order)-s.head)
	for _, id := range s.order[s.head:] {
		if d, ok := s.items[id]; ok {
			out = append(out, d)
		}
	}
	return out
}

// Pick 按 ID 顺序返回指定会话，忽略已不存在的 ID。
func (s *Store) Pick(ids []string) []*FlowDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*FlowDetail, 0, len(ids))
	for _, id := range ids {
		if d, ok := s.items[id]; ok {
			out = append(out, d)
		}
	}
	return out
}

// Delete 按 ID 删除会话，返回实际删除的 ID。
// 删除是精确操作而非清空，因此必须重建 order：map 无法保证顺序，
// 而 order 一旦出现空洞，ListSince 的"遇到不大于 since 即停"就会失效。
func (s *Store) Delete(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	deleted := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := s.items[id]; !ok {
			continue
		}
		delete(s.items, id)
		delete(s.ws, id)
		deleted = append(deleted, id)
	}
	if len(deleted) == 0 {
		return nil
	}

	gone := make(map[string]struct{}, len(deleted))
	for _, id := range deleted {
		gone[id] = struct{}{}
	}
	// 重建时只保留有效窗口，顺带把已淘汰的空洞清掉
	kept := make([]string, 0, len(s.order)-s.head)
	for _, id := range s.order[s.head:] {
		if _, ok := gone[id]; ok {
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	s.head = 0
	return deleted
}

// Clear 清空所有会话及其帧。
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = s.order[:0]
	s.head = 0
	s.items = make(map[string]*FlowDetail, s.max)
	s.ws = make(map[string]*frameSet)
}

// Len 返回当前会话数。
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order) - s.head
}

// Max 返回容量上限。
func (s *Store) Max() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.max
}

// Resize 调整容量上限，超出部分立即淘汰。
func (s *Store) Resize(max int) {
	if max <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.max = max
	if live := len(s.order) - s.head; live > max {
		s.evictLocked(live - max)
	}
}

// AppendFrame 追加一帧到指定会话，返回是否保留、当前总数与被丢弃的历史帧数。
// 会话已被淘汰时不接收新帧——否则帧存储会成为无主内存泄漏。
func (s *Store) AppendFrame(flowID string, f Frame) (kept bool, total, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.items[flowID]
	if !ok {
		return false, 0, 0
	}
	fs := s.ws[flowID]
	if fs == nil {
		fs = &frameSet{items: make([]Frame, 0, 16)}
		s.ws[flowID] = fs
	}
	if len(fs.items) >= maxFramesPerFlow || fs.bytes+f.Size > maxFrameBytesPerFlow {
		fs.dropped++
		return false, len(fs.items), fs.dropped
	}
	f.Index = len(fs.items)
	fs.items = append(fs.items, f)
	fs.bytes += f.Size
	d.Frames = len(fs.items)
	return true, len(fs.items), fs.dropped
}

// Frames 返回指定会话已捕获的帧副本与被丢弃数量。
// 返回副本而非切片本身：调用方（JSON 序列化、前端）与写入方并发，直接暴露会数据竞争。
func (s *Store) Frames(flowID string) ([]Frame, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fs := s.ws[flowID]
	if fs == nil {
		return nil, 0
	}
	out := make([]Frame, len(fs.items))
	copy(out, fs.items)
	return out, fs.dropped
}
