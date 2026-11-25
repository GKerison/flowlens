/**
 * 前端使用的数据模型。
 * 与后端 DTO 一一对应，字段名保持一致，避免运行时再做映射。
 * 后端定义位置：internal/capture/{flow,frames,har}.go、internal/ca/ca.go、
 * internal/config/config.go、internal/proxy/manager.go、internal/service/app.go
 */

export interface BodyView {
  kind: 'json' | 'html' | 'xml' | 'text' | 'image' | 'binary' | 'empty'
  mime: string
  size: number
  /** 实际使用的字符集，如 utf-8 / gb18030；非 UTF-8 时界面需要提示已转码 */
  encoding?: string
  truncated: boolean
  preview?: string
  base64?: string
}

export interface FlowBrief {
  seq: number
  id: string
  method: string
  scheme: string
  host: string
  /** 非默认端口时才有值（如 8080），用于还原完整 URL */
  port?: string
  path: string
  query: string
  statusCode: number
  contentType: string
  kind: string
  reqSize: number
  resSize: number
  durationMs: number
  startTime: string
  clientAddr: string
  serverAddr: string
  proto: string
  intercepted: boolean
  /** WebSocket/SSE 已捕获的消息数 */
  frames: number
  /** 由「请求重放」产生的会话 */
  replayed: boolean
  /** 命中的 Map Local 本地文件路径，非空表示响应被本地文件替换 */
  mapped?: string
  /** 命中的 Map Remote 目标，非空表示请求已被改写发往新地址 */
  remoted?: string
  error?: string
}

export interface FlowDetail extends FlowBrief {
  reqHeaders: Record<string, string[]>
  resHeaders: Record<string, string[]>
  reqBody?: BodyView
  resBody?: BodyView
}

/** 断点暂停时后端推送的上下文（见 proxy.BreakpointPayload）。 */
export interface BreakpointPayload {
  holdKey: string
  flowId: string
  stage: 'request' | 'response'
  method: string
  url: string
  headers: Array<{ name: string; value: string }>
  body: string
  bodyBinary: boolean
  bodyTruncated: boolean
  /** 仅 response 阶段有值 */
  statusCode?: number
  resHeaders?: Array<{ name: string; value: string }>
}

/** 用户对断点的处置（见 proxy.BreakpointDecision）。响应阶段的头编辑也用 headers 字段。 */
export interface BreakpointDecision {
  action: 'forward' | 'drop'
  method?: string
  url?: string
  /** null 表示沿用原头；数组（可为空）表示整体替换 */
  headers?: Array<{ name: string; value: string }> | null
  /** null 表示沿用原体；空串表示清空 */
  body?: string | null
  /** 仅 response 阶段有效；0 表示沿用 */
  statusCode?: number
}

/** WebSocket 单帧或 SSE 单个事件。 */
export interface Frame {
  index: number
  fromClient: boolean
  kind: 'text' | 'binary' | 'event'
  size: number
  text?: string
  base64?: string
  truncated: boolean
  timestamp: string
}

export interface FrameBatch {
  flowId: string
  frames: Frame[]
  total: number
  dropped: number
}

export interface HARExport {
  path: string
  entries: number
  bytes: number
}

export interface ProxyState {
  running: boolean
  addr: string
  paused: boolean
  flows: number
  startedAt: string
  lastError?: string
}

export interface CAInfo {
  subject: string
  commonName: string
  organization: string
  notBefore: string
  notAfter: string
  fingerprint: string
  pem: string
  path: string
}

export interface AppConfig {
  listenHost: string
  proxyPort: number
  certPort: number
  upstreamProxy: string
  insecureUpstream: boolean
  interceptTls: boolean
  skipDecryptHosts: string[]
  requireAuth: boolean
  authUsername: string
  authPassword: string
  maxFlows: number
  maxBodyBytes: number
  streamThreshold: number
  mapLocalRules: MapLocalRule[]
  mapRemoteRules: MapRemoteRule[]
  breakpointEnabled: boolean
  breakpointRequest: boolean
  breakpointResponse: boolean
  breakpointTimeout: number
  theme: string
  /** zh-CN | en-US，默认中文 */
  locale: string
  /** standard | spacious | simple，默认标准 */
  uiStyle: string
}

/** 界面风格预设。只影响展示层，不改变抓包与配置行为。 */
export type UIStyle = 'standard' | 'spacious' | 'simple'

/**
 * 一条 Map Remote 规则：命中的请求被改写到目标地址后真实发出。
 * pattern 语法与 MapLocalRule 一致；target 形如 https://host[:port][/prefix]。
 */
export interface MapRemoteRule {
  enabled: boolean
  pattern: string
  target: string
}

/**
 * 一条 Map Local 规则：命中的请求用本地文件内容替换响应体。
 * pattern 支持子串 / 通配符（* ?）/ 正则（re: 前缀），按数组顺序第一条命中生效。
 */
export interface MapLocalRule {
  enabled: boolean
  pattern: string
  file: string
  /** 0 表示沿用上游响应的状态码 */
  statusCode: number
  /** 空则按文件扩展名推断 */
  contentType: string
}

/** 编辑重放时的单个请求头。用数组而非对象，保留同名多值与顺序。 */
export interface HeaderPair {
  name: string
  value: string
}

/** 编辑重放的输入。body 为 null 表示沿用源请求体。 */
export interface ReplayEdit {
  method: string
  url: string
  headers: HeaderPair[]
  body: string | null
}

/** 含私钥的证书导出结果 */
export interface CAExportResult {
  path: string
  bytes: number
}

/** 本机信任库的安装目标（用户级 / 系统级）。 */
export interface TrustStore {
  name: string
  scope: 'user' | 'system'
  /** 空表示无需额外权限；否则是"需要管理员授权"这类提示 */
  requires: string
}

/** 根证书在本机的信任状态。 */
export interface TrustStatus {
  trusted: boolean
  /** 已信任时，是否装在系统级（对所有用户生效） */
  elevated: boolean
  detail: string
  store: TrustStore
  /** 平台是否支持提权安装（安装失败后可重试） */
  canElevate: boolean
  /** 默认安装将执行的命令，供用户预览 */
  command: string[]
  fingerprint: string
  checkedAt: string
}

/** 安装/卸载的执行结果。 */
export interface TrustResult {
  ok: boolean
  message: string
  command: string[]
  output: string
}

/** 证书分发服务状态与可访问地址。 */
export interface CertServerInfo {
  running: boolean
  addr: string
  /** 首选地址：局域网模式下是手机能访问的那个 */
  url: string
  urls: string[]
  /** 是否对局域网开放（监听 0.0.0.0） */
  lan: boolean
  port: number
  error?: string
}

/** 版本检测结果。assetUrl 由后端校验域名后才允许下载。 */
export interface UpdateInfo {
  currentVersion: string
  latestVersion: string
  notes: string
  publishedAt: string
  assetName: string
  assetUrl: string
  assetSize: number
  pageUrl: string
  upToDate: boolean
  checkedAt: string
}

/** 一键更新结果。manual=true 表示需要用户手动完成安装。 */
export interface UpdateResult {
  ok: boolean
  message: string
  path: string
  restarting: boolean
  manual: boolean
}

/** 端到端自检结果。 */
export interface SelfTestResult {
  ok: boolean
  message: string
  hint?: string
  targetUrl: string
  status: number
  durationMs: number
  flows: number
}

/** 系统代理接管状态。 */
export interface SysProxyStatus {
  /** 平台是否支持自动接管（Linux 非 GNOME 桌面为 false） */
  supported: boolean
  /** 系统代理当前是否指向 观流 */
  enabled: boolean
  /** 启动时检测到上次未正常恢复 */
  stale: boolean
  /** none / manual / auto */
  mode: string
  /** 系统当前生效的代理地址 */
  addr: string
  /** 观流 会设置成的地址 */
  target: string
  /** 修改系统设置是否必然触发授权 */
  needsAdmin: boolean
  /** 使用的机制，如 networksetup */
  mechanism: string
  platform: string
  detail: string
  /** 不支持自动接管时的手动配置步骤 */
  manual: string[]
  error?: string
  checkedAt: string
}

/** 接管/恢复的执行结果。 */
export interface SysProxyResult {
  ok: boolean
  changed: boolean
  message: string
  output?: string
  /** 实际执行的命令，供审计 */
  commands: string[]
  status: SysProxyStatus
}

export interface Bootstrap {
  state: ProxyState
  ca: CAInfo
  config: AppConfig
  trust: TrustStatus
  cert: CertServerInfo
  sysProxy: SysProxyStatus
  appDir: string
  configPath: string
  logDir: string
  version: string
  platform: string
  proxyUrl: string
  lanIps: string[]
  /** 启动自愈的结论（已自动恢复 / 需要人工恢复），只显示一次 */
  sysProxyNote?: string
}

export interface Filter {
  keyword: string
  method: string
  kind: string
  onlyErrors: boolean
  /** 关键字是否同时搜索响应体内容（后端扫描内存中的解码文本） */
  searchBody: boolean
}

export type Locale = 'zh-CN' | 'en-US'

/** 列表可排序的列。'' 表示保持时间序（后端推送顺序）。 */
export type SortKey = 'seq' | 'method' | 'host' | 'path' | 'status' | 'kind' | 'size' | 'duration'
