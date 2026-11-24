/**
 * 展示层格式化工具。
 *
 * 抽出来的原因：列表、分组列表、详情面板对"同一个值怎么显示"必须完全一致
 * （主机是否带端口、0 字节显示 '-' 还是 '0 B'），各写一份迟早会出现两处不一致。
 */
import type { BodyView, FlowBrief } from './types'

/** 默认端口省略、非默认端口保留，与浏览器地址栏行为一致。 */
export function displayHost(f: FlowBrief): string {
  if (!f.port) return f.host
  const isDefault =
    (f.scheme === 'http' && f.port === '80') || (f.scheme === 'https' && f.port === '443')
  return isDefault ? f.host : `${f.host}:${f.port}`
}

/** 完整 URL，规则与后端 capture.FlowBrief.URL() 保持一致。 */
export function flowURL(f: FlowBrief): string {
  return `${f.scheme}://${displayHost(f)}${f.path}${f.query ? '?' + f.query : ''}`
}

/** 方法配色类：未知方法统一归到 OTHER，避免样式表里漏一种就失去颜色。 */
export function methodClass(method: string): string {
  const known = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE']
  return `m-${known.includes(method) ? method : 'OTHER'}`
}

/** 状态码配色类：按百位分组，0 表示没有响应。 */
export function statusClass(code: number): string {
  if (code === 0) return 's-0'
  return `s-${Math.floor(code / 100)}`
}

/**
 * 字节数转可读大小。
 * empty 指定 0 时的占位：列表里用 '-' 更省空间，详情页需要明确的 '0 B'。
 */
export function formatSize(bytes: number, empty = '0 B'): string {
  if (!bytes) return empty
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

/** 耗时：1s 以下用毫秒，超过用秒，避免 12345 ms 这种要数位数的写法。 */
export function formatDuration(ms: number, empty = '-'): string {
  if (!ms) return empty
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(2)} s`
}

/** 把 JSON 文本格式化成缩进版本；解析失败（如被截断）则原样返回。 */
export function prettyJson(text?: string): string {
  if (!text) return ''
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

/** BodyView 的可复制文本：JSON 走格式化，其余原样。 */
export function bodyText(body?: BodyView): string {
  if (!body) return ''
  if (body.kind === 'json') return prettyJson(body.preview)
  return body.preview || ''
}

/** 头字段的有序视图。保留 values 数组是为了复制时能还原同名多值。 */
export interface HeaderRow {
  name: string
  values: string[]
  joined: string
}

/** 按名称排序，保证同一条会话多次查看时顺序稳定。 */
export function headerRows(headers?: Record<string, string[]>): HeaderRow[] {
  if (!headers) return []
  return Object.keys(headers)
    .sort((a, b) => a.localeCompare(b))
    .map((name) => {
      const values = headers[name] ?? []
      return { name, values, joined: values.join(', ') }
    })
}
