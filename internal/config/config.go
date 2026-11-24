// Package config 负责 观流 的配置定义、默认值、持久化与校验。
//
// 设计约束：
//  1. 配置只存"用户意图"，不存运行态（如是否正在抓包），运行态由 proxy.Manager 维护。
//  2. 所有写盘操作必须原子（临时文件 + rename），避免崩溃留下半截 JSON。
//  3. 读取时对历史配置做校验与纠偏，保证旧版本配置升级后仍可用。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// AppDirName 是应用在系统用户配置目录下的文件夹名。
const AppDirName = "FlowLens"

// 默认端口：8888 是抓包工具的惯例端口，8889 供证书分发页使用。
const (
	DefaultProxyPort = 8888
	DefaultCertPort  = 8889
)

// Config 是应用的完整配置。字段使用驼峰 JSON 命名，前端可直接消费。
type Config struct {
	// 网络与代理
	ListenHost       string `json:"listenHost"`       // 监听地址：0.0.0.0 允许局域网设备接入，127.0.0.1 仅本机
	ProxyPort        int    `json:"proxyPort"`        // 代理端口
	CertPort         int    `json:"certPort"`         // 证书分发页端口
	UpstreamProxy    string `json:"upstreamProxy"`    // 上游代理，形如 http://127.0.0.1:7890，空表示直连
	InsecureUpstream bool   `json:"insecureUpstream"` // 是否跳过上游证书校验（仅调试自签服务时使用）

	// 解密策略
	InterceptTLS     bool     `json:"interceptTls"`     // 是否解密 HTTPS；关闭后仅能看到 CONNECT 隧道
	SkipDecryptHosts []string `json:"skipDecryptHosts"` // 不解密的域名后缀，如 bank.com

	// 代理认证：LAN 模式下的最低安全要求
	RequireAuth  bool   `json:"requireAuth"`
	AuthUsername string `json:"authUsername"`
	AuthPassword string `json:"authPassword"`

	// 抓取与性能
	MaxFlows        int   `json:"maxFlows"`        // 内存中保留的最大会话数
	MaxBodyBytes    int64 `json:"maxBodyBytes"`    // 单条 body 保留上限，超出截断
	StreamThreshold int64 `json:"streamThreshold"` // 超过该大小的 body 不缓冲（内核参数）

	// 调试
	// MapLocalRules 是本地映射规则：命中的请求用本地文件内容替换响应体。
	// 与 Map Remote 的区别是"不发请求也不改请求"，纯前端替换，适合联调后端未就绪的接口。
	MapLocalRules []MapLocalRule `json:"mapLocalRules"`
	// MapRemoteRules 是远程映射规则：命中的请求被改写到目标地址后真实发出。
	// 与 Map Local 的分工：Local 替换"响应内容"，Remote 改写"请求去向"（切环境/换后端）。
	MapRemoteRules []MapRemoteRule `json:"mapRemoteRules"`

	// 断点：命中后暂停请求/响应，等用户编辑或放行。
	// 全局开关 + 阶段 + 超时，先不做按域名规则——全局开关已覆盖八成调试场景，
	// 规则粒度等真实需求出现再加（YAGNI）。
	BreakpointEnabled  bool `json:"breakpointEnabled"`
	BreakpointRequest  bool `json:"breakpointRequest"`  // 拦截请求阶段
	BreakpointResponse bool `json:"breakpointResponse"` // 拦截响应阶段
	BreakpointTimeout  int  `json:"breakpointTimeout"`  // 等待秒数，超时自动放行

	// 界面
	Theme   string `json:"theme"`   // system | light | dark
	Locale  string `json:"locale"`  // zh-CN | en-US，默认中文
	UIStyle string `json:"uiStyle"` // standard | spacious | simple，默认标准
}

// MapLocalRule 是一条本地映射规则。
//
// 匹配顺序即数组顺序，**第一条命中即生效**（便于用一条宽泛规则兜底、在上面叠特例）。
// Pattern 支持三种写法：
//   - 普通字符串：URL 包含该子串即命中（"api/users" 可匹配任意域名的该路径）；
//   - 通配符：`*` 匹配任意字符、`?` 匹配单个字符，整串必须完全匹配（"https://a.com/api/*"）；
//   - 正则：以 "re:" 开头，后面按 Go RE2 语法解析（"re:^https://a\\.com/v\\d+/user/\\d+$"）。
type MapLocalRule struct {
	Enabled     bool   `json:"enabled"`
	Pattern     string `json:"pattern"`
	File        string `json:"file"`        // 本地文件绝对路径，每次请求现读，改文件不用重启
	StatusCode  int    `json:"statusCode"`  // 0 表示沿用上游响应的状态码
	ContentType string `json:"contentType"` // 空则按文件扩展名推断
}

// MapRemoteRule 是一条远程映射规则：命中的请求改写到 Target 后真实发出。
//
// Pattern 语法与 MapLocalRule 完全一致（子串 / 通配 / re: 正则），匹配的也是完整 URL。
// Target 两种写法：
//   - 只写源（scheme://host[:port]）：仅改写域名/端口，路径与查询串原样保留
//     （"切环境"最常用：https://api.prod.com → https://api.beta.com:8443）；
//   - 带路径（scheme://host[:port]/prefix）：目标路径作为前缀拼在原路径之前
//     （https://old.com → https://new.com/v2 时，/api/users → /v2/api/users）。
type MapRemoteRule struct {
	Enabled bool   `json:"enabled"`
	Pattern string `json:"pattern"`
	Target  string `json:"target"` // 改写目标，形如 https://host[:port][/prefix]
}

// 断点等待的默认与边界。超时自动放行是刻意的：断点忘了关会把整个页面卡死，
// 与其让用户困惑"网络怎么断了"，不如到点放行并在界面上说明。
const (
	DefaultBreakpointTimeout = 120
	MinBreakpointTimeout     = 5
	MaxBreakpointTimeout     = 600
)

// Default 返回出厂配置。
//
// 默认监听 0.0.0.0：抓包最常见的场景是"手机/模拟器连电脑代理"，
// 只监听 127.0.0.1 会让首次使用必然走一遍改配置→重启的弯路。
// 局域网可达的安全风险由设置页的醒目提示与代理认证兜底；
// 已有用户的旧配置不受影响（磁盘上是什么就用什么）。
func Default() Config {
	return Config{
		ListenHost:        "0.0.0.0",
		ProxyPort:         DefaultProxyPort,
		CertPort:          DefaultCertPort,
		UpstreamProxy:     "",
		InsecureUpstream:  false,
		InterceptTLS:      true,
		SkipDecryptHosts:  []string{},
		RequireAuth:       false,
		AuthUsername:      "flowlens",
		AuthPassword:      "",
		MaxFlows:          5000,
		MaxBodyBytes:      2 << 20, // 2MB
		StreamThreshold:   5 << 20, // 5MB
		MapLocalRules:     []MapLocalRule{},
		MapRemoteRules:    []MapRemoteRule{},
		BreakpointTimeout: DefaultBreakpointTimeout,
		Theme:             "system",
		Locale:            "zh-CN",
		UIStyle:           "standard",
	}
}

// Validate 校验并纠偏配置。返回的错误表示无法自动修复的问题。
func (c *Config) Validate() error {
	var errs []string

	if c.ListenHost == "" {
		c.ListenHost = "0.0.0.0"
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		errs = append(errs, "代理端口必须在 1-65535 之间")
	}
	if c.CertPort < 1 || c.CertPort > 65535 {
		errs = append(errs, "证书服务端口必须在 1-65535 之间")
	}
	if c.ProxyPort == c.CertPort {
		errs = append(errs, "代理端口与证书服务端口不能相同")
	}
	if c.MaxFlows < 100 {
		c.MaxFlows = 100
	}
	if c.MaxFlows > 100000 {
		c.MaxFlows = 100000
	}
	if c.MaxBodyBytes < 64<<10 {
		c.MaxBodyBytes = 64 << 10
	}
	if c.MaxBodyBytes > 32<<20 {
		c.MaxBodyBytes = 32 << 20
	}
	if c.StreamThreshold < c.MaxBodyBytes {
		c.StreamThreshold = c.MaxBodyBytes
	}
	switch c.Theme {
	case "system", "light", "dark":
	default:
		c.Theme = "system"
	}
	switch c.Locale {
	case "zh-CN", "en-US":
	default:
		c.Locale = "zh-CN"
	}
	// 旧配置没有该字段时回落标准风格，保证升级后界面不变样
	switch c.UIStyle {
	case "standard", "spacious", "simple":
	default:
		c.UIStyle = "standard"
	}
	if c.UpstreamProxy != "" && !strings.Contains(c.UpstreamProxy, "://") {
		errs = append(errs, "上游代理地址需带协议，例如 http://127.0.0.1:7890")
	}
	// 去重、去空白，保证域名列表干净
	c.SkipDecryptHosts = normalizeHosts(c.SkipDecryptHosts)

	// 认证纠偏：勾了认证却没设密码等于没认证，静默关掉比让用户以为"已经安全"要好。
	// 保存路径上的明确报错由 service 层负责，这里只做启动/加载时的兜底。
	if c.RequireAuth && strings.TrimSpace(c.AuthPassword) == "" {
		c.RequireAuth = false
	}
	if strings.TrimSpace(c.AuthUsername) == "" {
		c.AuthUsername = "flowlens"
	}

	// Map Local 纠偏：缺匹配式或缺少文件时直接禁用该条，而不是让整份配置无法启动
	// （文件可能被用户临时删掉/移动，这种情况不该把应用卡在打不开的状态）。
	// 保存路径上的明确报错由 maplocal.ValidateRules 负责，那里才有上下文说清哪条错了。
	c.MapLocalRules = normalizeMapLocal(c.MapLocalRules)
	// Map Remote 纠偏：目标写错时禁用该条（原因同样由保存路径负责说清）
	c.MapRemoteRules = normalizeMapRemote(c.MapRemoteRules)

	// 断点纠偏：开关与阶段不一致、超时越界
	if c.BreakpointTimeout < MinBreakpointTimeout || c.BreakpointTimeout > MaxBreakpointTimeout {
		c.BreakpointTimeout = DefaultBreakpointTimeout
	}
	if c.BreakpointEnabled && !c.BreakpointRequest && !c.BreakpointResponse {
		// 开了总开关但一个阶段都没勾：静默视为只拦请求（界面勾选框会如实反映）
		c.BreakpointRequest = true
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "；"))
	}
	return nil
}

// normalizeMapLocal 清洗映射规则：去空白，丢掉完全空白的行。
// 只填了一半的规则（如刚写完匹配式还没选文件）要保留——用户只是还没填完，
// 直接删掉他的输入比留着更让人恼火；这类规则一律置为未启用，避免误命中。
func normalizeMapLocal(in []MapLocalRule) []MapLocalRule {
	out := make([]MapLocalRule, 0, len(in))
	for _, r := range in {
		r.Pattern = strings.TrimSpace(r.Pattern)
		r.File = strings.TrimSpace(r.File)
		r.ContentType = strings.TrimSpace(r.ContentType)
		if r.Pattern == "" && r.File == "" {
			continue // 点了"添加规则"就没再动的空行，留着没有意义
		}
		if r.Pattern == "" || r.File == "" {
			r.Enabled = false
		}
		if r.StatusCode < 0 || r.StatusCode > 599 {
			r.StatusCode = 0
		}
		out = append(out, r)
	}
	return out
}

// normalizeMapRemote 清洗远程映射规则，语义与 normalizeMapLocal 一致：
// 空行丢弃、半填保留但禁用，绝不因规则没填完而阻塞应用启动。
func normalizeMapRemote(in []MapRemoteRule) []MapRemoteRule {
	out := make([]MapRemoteRule, 0, len(in))
	for _, r := range in {
		r.Pattern = strings.TrimSpace(r.Pattern)
		r.Target = strings.TrimSpace(r.Target)
		if r.Pattern == "" && r.Target == "" {
			continue
		}
		if r.Pattern == "" || r.Target == "" {
			r.Enabled = false
		}
		out = append(out, r)
	}
	return out
}

// normalizeHosts 清洗域名列表：去空格、转小写、去空项、去重。
func normalizeHosts(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimPrefix(h, ".")
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// Manager 持有配置并提供并发安全的读写。
type Manager struct {
	path string
	dir  string
	mu   sync.RWMutex
	cfg  Config
	// rev 每次配置变化 +1，供上层做"配置变了就重建缓存"的廉价判断。
	// 用自增版本号而不是比较配置内容：调用方（如代理）不想为此存一份完整副本。
	rev atomic.Uint64
}

// NewManager 加载配置；文件不存在时写入默认配置。
func NewManager() (*Manager, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return NewManagerAt(filepath.Join(dir, AppDirName))
}

// NewManagerAt 在指定目录加载配置，供测试或便携模式使用。
func NewManagerAt(base string) (*Manager, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("创建配置目录 %s 失败: %w", base, err)
	}

	m := &Manager{
		path: filepath.Join(base, "config.json"),
		dir:  base,
		cfg:  Default(),
	}

	data, err := os.ReadFile(m.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// 首次启动：落盘默认配置，便于用户直接查看与编辑
		if err := writeAtomic(m.path, m.cfg); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("读取配置 %s 失败: %w", m.path, err)
	default:
		loaded := Default()
		if err := json.Unmarshal(data, &loaded); err != nil {
			// 配置损坏时不阻塞启动：备份原文件后回退默认值，避免用户彻底打不开应用
			_ = os.Rename(m.path, m.path+".broken")
			loaded = Default()
		}
		if err := loaded.Validate(); err != nil {
			return nil, fmt.Errorf("配置不合法: %w", err)
		}
		m.cfg = loaded
	}
	return m, nil
}

// Dir 返回应用数据根目录（配置、CA、日志都在其下）。
func (m *Manager) Dir() string { return m.dir }

// Revision 返回配置版本号。值变化即代表配置被改写，可用于重建派生缓存。
func (m *Manager) Revision() uint64 { return m.rev.Load() }

// ConfigPath 返回配置文件绝对路径。
func (m *Manager) ConfigPath() string { return m.path }

// Get 返回配置副本，调用方可随意修改而不影响内部状态。
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := m.cfg
	c.SkipDecryptHosts = append([]string(nil), m.cfg.SkipDecryptHosts...)
	c.MapLocalRules = append([]MapLocalRule(nil), m.cfg.MapLocalRules...)
	c.MapRemoteRules = append([]MapRemoteRule(nil), m.cfg.MapRemoteRules...)
	return c
}

// Update 以回调方式修改配置并立即落盘。
// 回调返回错误时放弃修改，保证"要么全成功，要么不变"。
func (m *Manager) Update(fn func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.cfg
	next.SkipDecryptHosts = append([]string(nil), m.cfg.SkipDecryptHosts...)
	next.MapLocalRules = append([]MapLocalRule(nil), m.cfg.MapLocalRules...)
	next.MapRemoteRules = append([]MapRemoteRule(nil), m.cfg.MapRemoteRules...)
	if err := fn(&next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := writeAtomic(m.path, next); err != nil {
		return err
	}
	m.cfg = next
	m.rev.Add(1)
	return nil
}

// writeAtomic 先写临时文件再 rename，避免写入中途崩溃导致配置文件损坏。
func writeAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	return nil
}
