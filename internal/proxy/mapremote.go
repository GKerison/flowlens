package proxy

import (
	"net/url"

	mitm "github.com/lqqyt2423/go-mitmproxy/proxy"

	"github.com/kerison/flowlens/internal/applog"
	"github.com/kerison/flowlens/internal/mapremote"
)

// applyMapRemote 在请求发出前检查远程映射规则，命中则整体改写请求 URL。
// 返回规则目标（供界面标注），未命中返回空串。
//
// 时序说明：内核在 Request 钩子之后用 f.Request.URL 构造外发请求，
// 因此这里改 URL 是**真正生效**的；host 变化时内核会自动改用独立连接池，
// 不会把新请求塞进指向旧主机的复用连接。
// 与断点的先后：先 Map Remote 再断点——用户在断点里看到并编辑的
// 是"将要真实发出"的最终 URL。
func (m *Manager) applyMapRemote(f *mitm.Flow) string {
	if f == nil || f.Request == nil || f.Request.URL == nil {
		return ""
	}
	res := m.mapRemoteResolver()
	if res.Len() == 0 {
		return ""
	}

	rawURL := f.Request.URL.String()
	rule, ok := res.Match(rawURL)
	if !ok {
		return ""
	}

	rewritten, err := mapremote.Rewrite(rule, rawURL)
	if err != nil || rewritten == "" || rewritten == rawURL {
		if err != nil {
			applog.Warnf("Map Remote 未生效（%s）：%v", rule.Pattern, err)
		}
		return ""
	}

	parsed, err := url.Parse(rewritten)
	if err != nil {
		applog.Warnf("Map Remote 改写结果无法解析（%s → %s）：%v", rawURL, rewritten, err)
		return ""
	}
	f.Request.URL = parsed
	return rule.Target
}

// mapRemoteResolver 返回当前配置对应的规则集，按配置版本号缓存（与 Map Local 同策略）。
func (m *Manager) mapRemoteResolver() *mapremote.Resolver {
	rev := m.cfg.Revision()

	m.mrMu.Lock()
	defer m.mrMu.Unlock()
	if m.mrInit && m.mrRev == rev {
		return m.mrRes
	}

	res, err := mapremote.New(m.cfg.Get().MapRemoteRules)
	if err != nil {
		// 规则写错不能影响抓包主流程，但必须留痕
		applog.Warnf("Map Remote 规则编译失败，已跳过问题项：%v", err)
	}
	m.mrRev, m.mrRes, m.mrInit = rev, res, true
	return res
}
