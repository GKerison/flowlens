package capture

import (
	"encoding/base64"
	"time"
)

// Frame 是 WebSocket 单帧或 SSE 单个事件的展示模型。
//
// 之所以与 FlowBrief 分开存储：一条长连接的帧数可能上万，若挂在会话详情里，
// 每次拉详情都要把整条流的历史全量序列化，界面必然卡顿。独立存储可以按需拉取。
type Frame struct {
	Index      int       `json:"index"` // 会话内从 0 开始的序号，便于前端定位
	FromClient bool      `json:"fromClient"`
	Kind       string    `json:"kind"` // text | binary | event
	Size       int       `json:"size"` // 原始字节数
	Text       string    `json:"text,omitempty"`
	Base64     string    `json:"base64,omitempty"`
	Truncated  bool      `json:"truncated"` // 单帧内容超限被裁切
	Timestamp  time.Time `json:"timestamp"`
}

// FrameBatch 是推送给前端的一批帧。
type FrameBatch struct {
	FlowID  string  `json:"flowId"`
	Frames  []Frame `json:"frames"`
	Total   int     `json:"total"`
	Dropped int     `json:"dropped"` // 因超出上限被丢弃的历史帧数
}

const (
	// maxFrameText 是单帧内联文本上限。超过则截断展示，避免一条巨型消息撑爆界面。
	maxFrameText = 32 << 10
	// maxFrameBinary 是单帧内联二进制的上限，超出只报告大小。
	maxFrameBinary = 64 << 10
)

// NewFrame 把内核的帧负载转换成展示模型。
// binary 为 true 表示 WebSocket 二进制帧（SSE 永远是文本）。
func NewFrame(index int, fromClient, binary bool, payload []byte, ts time.Time) Frame {
	f := Frame{
		Index:      index,
		FromClient: fromClient,
		Size:       len(payload),
		Timestamp:  ts,
		Kind:       "text",
	}

	if binary {
		f.Kind = "binary"
		if len(payload) <= maxFrameBinary {
			f.Base64 = base64.StdEncoding.EncodeToString(payload)
		} else {
			f.Truncated = true
		}
		return f
	}

	if len(payload) > maxFrameText {
		payload = payload[:maxFrameText]
		f.Truncated = true
	}
	// WebSocket 协议要求文本帧是 UTF-8，但现实中仍有服务端发 GBK，
	// 这里统一走字符集探测，代价很低（纯 UTF-8 时只做一次有效性检查）。
	f.Text, _ = DecodeText(payload, "text/plain")
	return f
}
