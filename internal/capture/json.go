package capture

import (
	"encoding/json"
	"time"
)

// 本文件实现 FlowLens 自有 JSON 导出格式。
//
// 与 HAR 的分工：
//   - HAR 是行业标准，用于喂给 Chrome DevTools、Charles、各类分析脚本；
//   - 本格式是"原样快照"，只服务 FlowLens 自己（回归对比、报障附件、脚本加工），
//     因此可以直白地暴露字段名，不必迁就 HAR 的 headersSize/first/last 之类历史包袱。
//
// 格式一旦发布就要考虑兼容：新增字段可以，重命名/删除字段必须升 version。

// JSONFormat 是当前导出格式的标识，格式发生不兼容变化时递增主版本。
const JSONFormat = "flowlens.flows/1"

// JSONExport 是导出文件的根结构。
type JSONExport struct {
	Format     string      `json:"format"`
	ExportedAt string      `json:"exportedAt"`
	Creator    JSONCreator `json:"creator"`
	Count      int         `json:"count"`
	Flows      []JSONFlow  `json:"flows"`
}

// JSONCreator 记录导出工具与版本，便于排查"哪一版导出的文件读不进来"。
type JSONCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// JSONFlow 是一条会话的完整快照。
// URL 是从 host/port/path/query 还原出来的，避免使用方各自拼错端口规则。
type JSONFlow struct {
	ID          string      `json:"id"`
	URL         string      `json:"url"`
	Method      string      `json:"method"`
	Scheme      string      `json:"scheme"`
	Host        string      `json:"host"`
	Port        string      `json:"port,omitempty"`
	Path        string      `json:"path"`
	Query       string      `json:"query,omitempty"`
	StatusCode  int         `json:"statusCode"`
	ContentType string      `json:"contentType,omitempty"`
	Kind        string      `json:"kind"`
	ReqSize     int64       `json:"reqSize"`
	ResSize     int64       `json:"resSize"`
	DurationMS  int64       `json:"durationMs"`
	StartTime   string      `json:"startTime"`
	ClientAddr  string      `json:"clientAddr,omitempty"`
	ServerAddr  string      `json:"serverAddr,omitempty"`
	Proto       string      `json:"proto,omitempty"`
	Intercepted bool        `json:"intercepted"`
	Frames      int         `json:"frames,omitempty"`
	Replayed    bool        `json:"replayed,omitempty"`
	Mapped      string      `json:"mapped,omitempty"` // 命中的本地映射规则，非空表示响应被替换过
	Error       string      `json:"error,omitempty"`
	Request     JSONMessage `json:"request"`
	Response    JSONMessage `json:"response"`
}

// JSONMessage 是单侧的报文。headers 用数组而非对象：
// 同名头（Set-Cookie、Via）会有多份，压成 map 就丢了顺序与重复项。
type JSONMessage struct {
	Headers []HARNameVal `json:"headers"`
	Body    *BodyView    `json:"body,omitempty"`
}

// BuildJSON 把会话转换为 FlowLens JSON 快照。
func BuildJSON(flows []*FlowDetail, version string) ([]byte, error) {
	out := JSONExport{
		Format:     JSONFormat,
		ExportedAt: time.Now().Format(time.RFC3339),
		Creator:    JSONCreator{Name: "FlowLens", Version: version},
		Count:      0,
		Flows:      make([]JSONFlow, 0, len(flows)),
	}
	for _, f := range flows {
		if f == nil {
			continue
		}
		out.Flows = append(out.Flows, buildJSONFlow(f))
	}
	out.Count = len(out.Flows)
	return json.MarshalIndent(out, "", "  ")
}

func buildJSONFlow(f *FlowDetail) JSONFlow {
	return JSONFlow{
		ID:          f.ID,
		URL:         f.URL(),
		Method:      f.Method,
		Scheme:      f.Scheme,
		Host:        f.Host,
		Port:        f.Port,
		Path:        f.Path,
		Query:       f.Query,
		StatusCode:  f.StatusCode,
		ContentType: f.ContentType,
		Kind:        f.Kind,
		ReqSize:     f.ReqSize,
		ResSize:     f.ResSize,
		DurationMS:  f.DurationMS,
		StartTime:   f.StartTime.Format(time.RFC3339Nano),
		ClientAddr:  f.ClientAddr,
		ServerAddr:  f.ServerAddr,
		Proto:       f.Proto,
		Intercepted: f.Intercepted,
		Frames:      f.Frames,
		Replayed:    f.Replayed,
		Mapped:      f.Mapped,
		Error:       f.Error,
		Request: JSONMessage{
			Headers: toNameVals(f.ReqHeaders),
			Body:    f.ReqBody,
		},
		Response: JSONMessage{
			Headers: toNameVals(f.ResHeaders),
			Body:    f.ResBody,
		},
	}
}
