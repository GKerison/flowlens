// Package maplocal 实现 Map Local：把命中的请求，响应体替换成磁盘上的本地文件。
//
// 与"改包"类功能的分工：
//   - 请求仍然真实发出（可以看到服务端返回的状态码/响应头），只替换响应体；
//   - 每次请求现读文件，所以"改一行 mock 数据 → 刷新页面"即可生效，不必重启代理；
//   - 匹配与加载都在本包，proxy 包只负责在合适的时机调用，便于单测。
//
// 已知边界：响应体超过 StreamLargeBodies 时内核不缓冲、不进 Response 钩子，此时规则不生效。
package maplocal

import (
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kerison/flowlens/internal/config"
)

// MaxFileSize 是单个映射文件的上限。
// 取 32MB 与 body 保留上限的最高档一致：再大的文件当成 mock 基本是误用，
// 而且一次性读进内存的代价远高于"从上游拿"。
const MaxFileSize = 32 << 20

// Matcher 判断完整 URL 是否命中。
type Matcher func(rawURL string) bool

// Compile 把用户写的匹配式编译成匹配函数。
//
// 三种写法（顺序即优先级）：
//  1. "re:xxx" —— 正则，Go RE2 语法，不做锚定，由用户自己写 ^ $；
//  2. 含 * 或 ? —— 通配符，整串完全匹配；
//  3. 其他 —— 子串包含。
//
// 之所以保留"子串"这种最松的形式：调试时最常做的事是"把这个接口 mock 掉"，
// 手边只有一个路径片段，逼用户补全 scheme+host 只会让人放弃使用。
func Compile(pattern string) (Matcher, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return nil, errors.New("匹配式不能为空")
	}

	if rest, ok := strings.CutPrefix(p, "re:"); ok {
		re, err := regexp.Compile(strings.TrimSpace(rest))
		if err != nil {
			return nil, fmt.Errorf("正则写法有误: %w", err)
		}
		return re.MatchString, nil
	}

	if strings.ContainsAny(p, "*?") {
		re, err := regexp.Compile(globToRegexp(p))
		if err != nil {
			return nil, fmt.Errorf("通配式写法有误: %w", err)
		}
		return re.MatchString, nil
	}

	sub := p
	return func(raw string) bool { return strings.Contains(raw, sub) }, nil
}

// globToRegexp 把 * ? 转成正则，其余字符按字面量转义。
// 遍历 rune 而不是 byte：中文路径当作 byte 切会被拆成半个字符，QuoteMeta 之后无法匹配。
func globToRegexp(p string) string {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range p {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// Rule 是编译后的规则。
type Rule struct {
	Pattern     string
	File        string
	StatusCode  int
	ContentType string
	match       Matcher
}

// Resolver 持有一组按顺序生效的规则。
type Resolver struct {
	rules []Rule
}

// New 编译规则集。返回的 Resolver 只包含编译成功的规则，
// 失败项会聚合成一个 error 返回——调用方应当把它记进日志，而不是丢掉：
// "规则写了但永远不生效"是最难排查的一类问题。
func New(rules []config.MapLocalRule) (*Resolver, error) {
	res := &Resolver{rules: make([]Rule, 0, len(rules))}
	var errs []string
	for i, r := range rules {
		if !r.Enabled {
			continue
		}
		m, err := Compile(r.Pattern)
		if err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 条（%s）：%v", i+1, r.Pattern, err))
			continue
		}
		res.rules = append(res.rules, Rule{
			Pattern:     r.Pattern,
			File:        r.File,
			StatusCode:  r.StatusCode,
			ContentType: r.ContentType,
			match:       m,
		})
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

// Load 读取规则对应的文件，并决定响应用的 Content-Type。
func Load(rule Rule) (body []byte, contentType string, err error) {
	path := strings.TrimSpace(rule.File)
	if path == "" {
		return nil, "", errors.New("没有指定本地文件")
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("本地文件不存在：%s", path)
		}
		return nil, "", fmt.Errorf("无法读取本地文件：%w", err)
	}
	if st.IsDir() {
		return nil, "", fmt.Errorf("%s 是目录，请指定具体文件", path)
	}
	if st.Size() > MaxFileSize {
		return nil, "", fmt.Errorf("文件过大（%d MB），上限 %d MB", st.Size()>>20, MaxFileSize>>20)
	}

	body, err = os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("读取本地文件失败：%w", err)
	}
	return body, detectContentType(rule, path), nil
}

// detectContentType 依次取：规则显式指定 → 扩展名推断 → 二进制兜底。
func detectContentType(rule Rule, path string) string {
	if ct := strings.TrimSpace(rule.ContentType); ct != "" {
		return ct
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	// mime.TypeByExtension 依赖系统 mime.types，Windows 上对前端常见类型覆盖不全，
	// 这里补一份最小映射，保证 mock .js/.json 时浏览器按正确类型解析
	if ct, ok := builtinTypes[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

var builtinTypes = map[string]string{
	".json":  "application/json",
	".js":    "application/javascript",
	".mjs":   "application/javascript",
	".css":   "text/css",
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".xml":   "application/xml",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".mp4":   "video/mp4",
	".webm":  "video/webm",
}

// ValidateRules 在保存配置前检查规则，返回第一条问题的完整描述。
//
// 与 config.Validate 的分工：那里只做"明显没意义就禁用"的静默纠偏，
// 这里必须报错——用户点保存时以为规则生效了，实际却匹配不上/读不到文件，
// 是这类工具最典型的信任崩塌点。
func ValidateRules(rules []config.MapLocalRule) error {
	for i, r := range rules {
		label := fmt.Sprintf("第 %d 条规则", i+1)
		if strings.TrimSpace(r.Pattern) == "" && strings.TrimSpace(r.File) == "" && !r.Enabled {
			continue // 界面上新增但还没填的空行，允许保存
		}
		if strings.TrimSpace(r.Pattern) == "" {
			return fmt.Errorf("%s：匹配式不能为空", label)
		}
		if _, err := Compile(r.Pattern); err != nil {
			return fmt.Errorf("%s：%v", label, err)
		}
		if strings.TrimSpace(r.File) == "" {
			return fmt.Errorf("%s：请指定本地文件", label)
		}
		if r.StatusCode != 0 && (r.StatusCode < 100 || r.StatusCode > 599) {
			return fmt.Errorf("%s：状态码需在 100-599 之间（0 表示沿用上游）", label)
		}
		// 未启用的规则只校验格式，不要求文件存在：用户可能先写下路径再准备文件
		if !r.Enabled {
			continue
		}
		st, err := os.Stat(strings.TrimSpace(r.File))
		if err != nil {
			return fmt.Errorf("%s：本地文件不可用（%s）", label, r.File)
		}
		if st.IsDir() {
			return fmt.Errorf("%s：%s 是目录，请指定具体文件", label, r.File)
		}
	}
	return nil
}
