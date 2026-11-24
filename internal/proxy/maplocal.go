package proxy

import (
	"net/http"
	"strconv"

	mitm "github.com/lqqyt2423/go-mitmproxy/proxy"

	"github.com/kerison/flowlens/internal/applog"
	"github.com/kerison/flowlens/internal/maplocal"
)

// applyMapLocal 在响应读取完成后检查本地映射规则，命中则整段替换响应。
// 返回命中的本地文件路径（供界面标注），未命中返回空串。
//
// 时序说明：内核在 Response 钩子之后才把响应写回客户端，
// 因此这里改 Response.Body / Header / StatusCode 是**真正生效**的，
// 而不只是改了一份记录。
func (m *Manager) applyMapLocal(f *mitm.Flow) string {
	if f == nil || f.Request == nil || f.Response == nil || f.Request.URL == nil {
		return ""
	}
	res := m.mapLocalResolver()
	if res.Len() == 0 {
		return ""
	}

	rule, ok := res.Match(f.Request.URL.String())
	if !ok {
		return ""
	}

	body, contentType, err := maplocal.Load(rule)
	if err != nil {
		// 文件被删/无权限时不要让请求失败：继续走上游响应，用户至少还能抓到真包
		applog.Warnf("Map Local 未生效（%s）：%v", rule.Pattern, err)
		return ""
	}

	if rule.StatusCode > 0 {
		f.Response.StatusCode = rule.StatusCode
	}
	h := f.Response.Header
	if h == nil {
		h = make(http.Header)
		f.Response.Header = h
	}
	// 本地文件是明文：上游若是 gzip/br 压缩响应，保留 Content-Encoding 会让客户端解码失败
	h.Del("Content-Encoding")
	h.Del("Transfer-Encoding")
	h.Set("Content-Type", contentType)
	// 长度必须显式重写。内核写回客户端时用的是这里的 Header，
	// 沿用上游长度会导致响应体被截断或客户端一直等数据。
	h.Set("Content-Length", strconv.Itoa(len(body)))
	f.Response.Body = body

	return rule.File
}

// mapLocalResolver 返回当前配置对应的规则集，按配置版本号缓存。
// 每次请求都重新编译正则的代价没有必要——Map Local 是逐响应触发的热路径。
func (m *Manager) mapLocalResolver() *maplocal.Resolver {
	rev := m.cfg.Revision()

	m.mlMu.Lock()
	defer m.mlMu.Unlock()
	if m.mlInit && m.mlRev == rev {
		return m.mlRes
	}

	res, err := maplocal.New(m.cfg.Get().MapLocalRules)
	if err != nil {
		// 规则写错不能影响抓包主流程，但必须留痕：
		// 否则用户只会看到"规则明明开了却不生效"，无从下手
		applog.Warnf("Map Local 规则编译失败，已跳过问题项：%v", err)
	}
	m.mlRev, m.mlRes, m.mlInit = rev, res, true
	return res
}
