/**
 * 轻量 i18n 核心。
 *
 * 设计取舍：中文原文即 key。观流 是中文团队维护的工具，文案散落在十几个组件里，
 * 再给每条编一个语义 key（如 traffic.filter.includeBody）会让"改一句文案"变成改三处，
 * 且 key 与显示值极易腐化。这里让中文原文直接当 key，en-US.ts 集中维护译文。
 *
 * 模板里用 `t('原文', { 参数 })`；参数占位符形如 `{count}`，统一替换。
 */
import type { Locale } from '../types'
import { zhCN } from './zh-CN'
import { enUS } from './en-US'

export const dictionaries: Record<Locale, Record<string, string>> = {
  'zh-CN': zhCN,
  'en-US': enUS,
}

/** 模块级当前语言，供非组件上下文（如后端错误转文案）使用。 */
let currentLocale: Locale = 'zh-CN'

export function setCurrentLocale(locale: Locale): void {
  currentLocale = locale
}

export function getCurrentLocale(): Locale {
  return currentLocale
}

/** 按指定语言翻译。中文原文在未收录时原样返回（永远有可读文案）。 */
export function translate(
  locale: Locale,
  text: string,
  params?: Record<string, string | number>,
): string {
  let out = text
  if (locale === 'en-US') {
    const mapped = enUS[text]
    if (mapped !== undefined) out = mapped
    else if (import.meta.env.DEV) console.warn(`[i18n] 缺少英文译文: ${text}`)
  }
  if (params) {
    for (const [key, value] of Object.entries(params)) {
      out = out.replaceAll(`{${key}}`, String(value))
    }
  }
  return out
}
