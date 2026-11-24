/**
 * 组件侧 i18n 入口。
 *
 * t() 读取 ui store 的 locale（响应式），因此切换语言时所有用到 t() 的模板
 * 会自动重新渲染，不需要刷新页面或手动广播事件。
 */
import { useUiStore } from './stores/ui'
import { translate } from './locales'

export function useI18n(): {
  t: (text: string, params?: Record<string, string | number>) => string
} {
  const ui = useUiStore()
  return {
    t(text: string, params?: Record<string, string | number>): string {
      return translate(ui.locale, text, params)
    },
  }
}
