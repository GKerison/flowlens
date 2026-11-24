// Package events 集中定义前后端通信的事件名。
//
// 独立成包的原因：事件名同时被 proxy（发送方）与 service（注册类型）使用，
// 放在任一业务包里都会造成循环依赖。
//
// 命名规范：统一前缀 flowlens:，动词过去式或名词，全小写短横线分隔。
package events

const (
	// EventFlows 批量推送新增会话（FlowBrief 数组），100ms 合并一次。
	EventFlows = "flowlens:flows"
	// EventFlowsCleared 会话被清空。
	EventFlowsCleared = "flowlens:flows-cleared"
	// EventFlowsDeleted 指定会话被删除，载荷为被删除的 ID 数组。
	EventFlowsDeleted = "flowlens:flows-deleted"
	// EventProxyState 代理运行状态变化。
	EventProxyState = "flowlens:proxy-state"
	// EventFrames 批量推送 WebSocket/SSE 新帧，载荷为 capture.FrameBatch。
	EventFrames = "flowlens:frames"
	// EventAppError 需要提示用户的错误。
	EventAppError = "flowlens:error"
	// EventBreakpoint 断点暂停，载荷为 proxy.BreakpointPayload。
	// 每条断点一条事件；用户放行/取消经 ResolveBreakpoint 回传，无完成事件。
	EventBreakpoint = "flowlens:breakpoint"
)
