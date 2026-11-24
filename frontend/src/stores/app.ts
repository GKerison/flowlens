/**
 * 应用装配：初始化与后端事件订阅。
 *
 * 全局状态已按域拆到三个 Pinia store：
 *  - useUiStore      界面偏好与瞬时反馈（主题/toast/确认框/页签/面板）
 *  - useTrafficStore 会话列表、选择、详情、帧、导出与重放
 *  - useConfigStore  代理状态、配置、证书、信任、系统代理、自检
 *
 * 本文件只做"接线"：拉首屏数据、订阅后端事件并分发到对应 store。
 * 组件请直接使用各 store，不要 import 这里（init 由 App.vue 的 onMounted 调用）。
 */
import { Events } from '@wailsio/runtime'
import { useI18n } from '../i18n'
// 绑定由 `wails3 generate bindings` 生成，勿手工修改
import { App as Backend } from '../../bindings/github.com/kerison/flowlens/internal/service'
import { FlowlensFlows, FlowlensFrames, FlowlensProxyState } from '../../bindings/events'
import type { BreakpointPayload, FlowBrief, FrameBatch, ProxyState } from '../types'
import { useConfigStore } from './config'
import { toMessage } from './helpers'
import { useTrafficStore } from './traffic'
import { useUiStore } from './ui'

let eventsBound = false

/** 初始化：拉首屏数据并订阅后端事件。由 App.vue 的 onMounted 调用一次。 */
export async function init(): Promise<void> {
  const ui = useUiStore()
  const config = useConfigStore()
  const traffic = useTrafficStore()
  const { t } = useI18n()
  try {
    const bs = (await Backend.Bootstrap()) as unknown as Parameters<
      typeof config.applyBootstrap
    >[0]
    config.applyBootstrap(bs)

    // Go 的空切片序列化后可能是 null，统一兜底成空数组，避免下游判空散落各处
    const flows = (await Backend.ListFlows(0)) as unknown as FlowBrief[] | null
    traffic.replaceFlows(flows)
    bindEvents()
    ui.loaded = true
  } catch (err) {
    ui.showToast(t('初始化失败：{message}', { message: toMessage(err) }), 'error')
  }
}

/** 订阅后端推送的事件。重复调用不会重复绑定。 */
function bindEvents(): void {
  if (eventsBound) return
  eventsBound = true

  const config = useConfigStore()
  const traffic = useTrafficStore()

  Events.On(FlowlensFlows, (event: { data: unknown }) => {
    traffic.applyFlowEvents(event.data as FlowBrief[] | null)
  })

  Events.On(FlowlensFrames, (event: { data: unknown }) => {
    traffic.applyFrameEvents(event.data as FrameBatch | null)
  })

  Events.On(FlowlensProxyState, (event: { data: unknown }) => {
    const next = event.data as ProxyState | null
    if (!next) return
    config.proxy = next
  })

  Events.On('flowlens:flows-cleared', () => {
    traffic.resetFlows()
  })

  Events.On('flowlens:flows-deleted', (event: { data: unknown }) => {
    const ids = event.data as string[] | null
    if (!Array.isArray(ids)) return
    traffic.removeFlows(ids)
  })

  // 断点挂起：payload 直接进 ui 队列，由 BreakpointDialog 逐条处置
  Events.On('flowlens:breakpoint', (event: { data: unknown }) => {
    const p = event.data as BreakpointPayload | null
    if (!p?.holdKey) return
    useUiStore().enqueueBreakpoint(p)
  })
}
