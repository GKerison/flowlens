/**
 * UI 域：主题、toast、确认框、视图偏好（列表形态 / 详情页签 / 设置面板）。
 *
 * 只放"界面自身的状态与瞬时反馈"，不掺业务数据；
 * 业务数据分别在 useTrafficStore（会话）与 useConfigStore（配置与连接）。
 */
import { defineStore } from 'pinia'
import { Clipboard } from '@wailsio/runtime'
import type { BreakpointPayload, Locale, UIStyle } from '../types'
import { useI18n } from '../i18n'
import { setCurrentLocale } from '../locales'

export type ToastKind = 'info' | 'error' | 'success'
export type ThemeMode = 'system' | 'light' | 'dark'
export type ListMode = 'list' | 'group'
export type DetailTab = 'overview' | 'request' | 'response' | 'preview' | 'frames'
export type SettingsSection = 'general' | 'cert'

interface Toast {
  text: string
  kind: ToastKind
}

interface ConfirmBox {
  open: boolean
  title: string
  message: string
  confirmText: string
  /** 破坏性操作用红色主按钮，避免眼睛跟普通确认混在一起 */
  danger: boolean
  resolve: ((ok: boolean) => void) | null
}

let toastTimer: number | undefined

export const useUiStore = defineStore('ui', {
  state: () => ({
    /** 首屏数据是否加载完成（Bootstrap + ListFlows + 事件订阅）。 */
    loaded: false,
    theme: 'system' as ThemeMode,
    /** 界面语言，默认中文；切换后所有 t() 模板立即重渲染。 */
    locale: 'zh-CN' as Locale,
    /** 界面风格预设：标准 / 大屏舒展 / 简易极简。只影响展示层。 */
    style: 'standard' as UIStyle,
    toast: null as Toast | null,
    /**
     * 应用内确认框。不用 window.confirm：macOS 的 WKWebView 不实现原生 confirm
     * 面板，调用直接返回 false 且不报错，表现为「点了清空/删除什么都没发生」。
     */
    confirm: {
      open: false,
      title: '确认操作',
      message: '',
      confirmText: '确定',
      danger: false,
      resolve: null,
    } as ConfirmBox,
    /** 列表形态：平铺 / 按域名分组。跨页面切换保持用户的选择。 */
    listMode: 'list' as ListMode,
    /**
     * 详情页签。跨会话切换保持——排查时经常要连续看多条的"响应"或"预览"，
     * 每次切会话都跳回概览等于逼用户重复点一次。
     */
    detailTab: 'overview' as DetailTab,
    /** 设置面板：开合与当前分区。 */
    settingsOpen: false,
    settingsSection: 'general' as SettingsSection,
    /**
     * 等待处置的断点队列。多条同时挂起时排队处理，先到先显；
     * 处置（放行/取消）后立即出队，由后端继续放行对应连接。
     */
    breakpoints: [] as BreakpointPayload[],
  }),

  actions: {
    /** 显示轻提示（自动消失）。 */
    showToast(text: string, kind: ToastKind = 'info'): void {
      this.toast = { text, kind }
      window.clearTimeout(toastTimer)
      toastTimer = window.setTimeout(() => {
        this.toast = null
      }, 3200)
    },

    /** 弹出确认框，返回用户是否确认。同时只处理一个请求，重复调用按取消处理。 */
    confirmAction(opts: {
      message: string
      title?: string
      confirmText?: string
      danger?: boolean
    }): Promise<boolean> {
      if (this.confirm.open) return Promise.resolve(false)
      const { t } = useI18n()
      this.confirm.title = opts.title ?? t('确认操作')
      this.confirm.message = opts.message
      this.confirm.confirmText = opts.confirmText ?? t('确定')
      this.confirm.danger = opts.danger ?? false
      this.confirm.open = true
      return new Promise<boolean>((resolve) => {
        this.confirm.resolve = resolve
      })
    },

    /** 关闭确认框并给出结论。resolve 只触发一次。 */
    settleConfirm(ok: boolean): void {
      const resolve = this.confirm.resolve
      this.confirm.open = false
      this.confirm.resolve = null
      resolve?.(ok)
    },

    /** 切换主题（跟随系统 / 亮 / 暗）。 */
    setTheme(theme: ThemeMode): void {
      this.theme = theme
      this.applyTheme()
    },

    /** 按 theme 与系统偏好落到文档根节点的 data-theme。 */
    applyTheme(): void {
      const want = this.theme
      const dark =
        want === 'dark' ||
        (want === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches)
      document.documentElement.dataset.theme = dark ? 'dark' : 'light'
    },

    /** 切换界面语言（中文 / 英文），同步 HTML lang 与非组件上下文的当前语言。 */
    setLocale(locale: Locale): void {
      this.locale = locale
      document.documentElement.lang = locale
      setCurrentLocale(locale)
    },

    /** 切换界面风格：落到文档根节点，所有布局样式通过 data-style 响应。 */
    setStyle(style: UIStyle): void {
      this.style = style
      document.documentElement.dataset.style = style
    },

    openSettings(section: SettingsSection = 'general'): void {
      this.settingsSection = section
      this.settingsOpen = true
    },

    closeSettings(): void {
      this.settingsOpen = false
    },

    /** 断点挂起：入队等待用户处置。 */
    enqueueBreakpoint(p: BreakpointPayload): void {
      // 同一 hold 重复推送（理论上不会）按幂等处理
      if (this.breakpoints.some((b) => b.holdKey === p.holdKey)) return
      this.breakpoints.push(p)
    },

    /** 断点处置完成：出队。 */
    settleBreakpoint(holdKey: string): void {
      this.breakpoints = this.breakpoints.filter((b) => b.holdKey !== holdKey)
    },

    /**
     * 复制文本到剪贴板，按可用性依次降级：
     * Wails 原生（桌面端最可靠）→ navigator.clipboard（浏览器 HTTPS/localhost）
     * → 隐藏 textarea + execCommand（老 WebView 兜底）。
     * WKWebView / 无头环境里 navigator.clipboard 常因焦点或权限静默失败，
     * 所以不能只依赖它。
     */
    async copyText(text: string, label?: string): Promise<boolean> {
      const { t } = useI18n()
      const copiedLabel = label ?? t('已复制')
      try {
        await Clipboard.SetText(text)
        this.showToast(copiedLabel, 'success')
        return true
      } catch {
        // 桌面端必成，失败说明跑在浏览器（server 模式），继续走 Web 通道
      }
      try {
        await navigator.clipboard.writeText(text)
        this.showToast(copiedLabel, 'success')
        return true
      } catch {
        // 继续走 execCommand 兜底
      }
      try {
        const ta = document.createElement('textarea')
        ta.value = text
        ta.style.position = 'fixed'
        ta.style.opacity = '0'
        document.body.appendChild(ta)
        ta.select()
        const ok = document.execCommand('copy')
        ta.remove()
        if (ok) {
          this.showToast(copiedLabel, 'success')
          return true
        }
      } catch {
        // 落到统一失败提示
      }
      this.showToast(t('复制失败，请手动选择文本'), 'error')
      return false
    },
  },
})
