// Package mapremote 实现 Map Remote：把命中的请求改写到目标地址后真实发出。
//
// 与 Map Local 的分工：
//   - Local 替换"响应内容"，请求仍然发给原服务器；
//   - Remote 改写"请求去向"（换域名/换端口/加路径前缀），响应来自新目标。
//
// 匹配语法复用 maplocal.Compile（子串 / 通配 / re: 正则），保证用户只需学一套写法。
// 改写发生在内核的 Request 钩子阶段：修改 f.Request.URL 会真实作用于外发请求。
package mapremote

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/maplocal"
)

// Rule 是编译后的远程映射规则。
type Rule struct {
	Pattern string
	Target  string
	match   maplocal.Matcher
}

// Resolver 持有一组按顺序生效的规则，第一条命中即生效。
type Resolver struct {
	rules []Rule
}

// New 编译规则集。失败项聚合进 error 返回，调用方应记日志而不是静默丢弃：
// "规则写了但永远不生效"是最难排查的一类问题。
func New(rules []config.MapRemoteRule) (*Resolver, error) {
	res := &Resolver{rules: make([]Rule, 0, len(rules))}
	var errs []string
	for i, r := range rules {
		if !r.Enabled {
			continue
		}
		m, err := maplocal.Compile(r.Pattern)
		if err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 条（%s）：%v", i+1, r.Pattern, err))
			continue
		}
		if _, err := parseTarget(r.Target); err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 条（目标 %s）：%v", i+1, r.Target, err))
			continue
		}
		res.rules = append(res.rules, Rule{Pattern: r.Pattern, Target: r.Target, match: m})
	}
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "；"))
	}
	return res, nil
}

// Len 返回可用规则数。
func (r *Resolver) Len() int {
	if r == nil {
		return 0
	}
	return len(r.rules)
}

// Match 返回第一条命中的规则。
func (r *Resolver) Match(rawURL string) (Rule, bool) {
	if r == nil {
		return Rule{}, false
	}
	for _, rule := range r.rules {
		if rule.match(rawURL) {
			return rule, true
		}
	}
	return Rule{}, false
}

// parseTarget 校验目标写法。要求 http/https 且有主机名；路径可选。
func parseTarget(target string) (*url.URL, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return nil, errors.New("目标不能为空")
	}
	u, err := url.Parse(t)
	if err != nil {
		return nil, fmt.Errorf("目标 URL 无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("目标需以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return nil, errors.New("目标缺少主机名，例如 https://api.example.com")
	}
	return u, nil
}

// Rewrite 按规则改写原始 URL。
//
// 语义：
//   - Target 无路径 → 只替换 scheme://host[:port]，路径与查询串原样保留；
//   - Target 带路径 → 目标路径作为前缀拼在原路径之前（原路径以 / 开头时不会出现双斜杠）。
//
// 返回改写后的完整 URL 字符串；解析失败返回空串与错误（调用方记日志后放行原请求）。
func Rewrite(rule Rule, rawURL string) (string, error) {
	orig, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("原始 URL 无法解析: %w", err)
	}
	target, err := parseTarget(rule.Target)
	if err != nil {
		return "", err
	}

	orig.Path = ensureLeadingSlash(orig.Path)
	if strings.Trim(target.Path, "/") == "" {
		// 目标不带路径：仅换源
		orig.Scheme, orig.Host = target.Scheme, target.Host
	} else {
		// 目标带路径：换源 + 路径前缀
		orig.Scheme, orig.Host = target.Scheme, target.Host
		prefix := strings.TrimSuffix(target.Path, "/")
		orig.Path = prefix + orig.Path
	}
	return orig.String(), nil
}

// ensureLeadingSlash 保证路径以 / 开头。空路径按 "/" 处理，
// 否则 url.Parse("https://host") 出来的 Path 是 ""，拼前缀会得到 "v2foo" 这种畸形路径。
func ensureLeadingSlash(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// ValidateRules 在保存配置前检查规则，返回第一条问题的完整描述。
// 语义与 maplocal.ValidateRules 一致：未启用的规则只校验格式。
func ValidateRules(rules []config.MapRemoteRule) error {
	for i, r := range rules {
		label := fmt.Sprintf("远程映射第 %d 条", i+1)
		if strings.TrimSpace(r.Pattern) == "" && strings.TrimSpace(r.Target) == "" && !r.Enabled {
			continue // 界面上新增但还没填的空行，允许保存
		}
		if strings.TrimSpace(r.Pattern) == "" {
			return fmt.Errorf("%s：匹配式不能为空", label)
		}
		if _, err := maplocal.Compile(r.Pattern); err != nil {
			return fmt.Errorf("%s：%v", label, err)
		}
		if _, err := parseTarget(r.Target); err != nil {
			return fmt.Errorf("%s：%v", label, err)
		}
	}
	return nil
}
