/**
 * 流量域：会话列表、选择、详情、帧、过滤与导出/重放。
 *
 * 只关心「抓到的流量」这件事；代理连接与配置在 useConfigStore，
 * 界面反馈（toast/确认框/页签偏好）在 useUiStore。
 *
 * 约定：
 *  - 所有后端调用集中在 actions，组件不直接 import 绑定模块；
 *  - 错误统一转成可读文案并通过 toast 呈现，不向组件抛异常（重放弹窗除外，
 *    它需要就地展示失败原因，由调用方 catch）。
 */
import { defineStore } from 'pinia'
import { Dialogs } from '@wailsio/runtime'
import { useI18n } from '../i18n'
// 绑定由 `wails3 generate bindings` 生成，勿手工修改
import { App as Backend } from '../../bindings/github.com/kerison/flowlens/internal/service'
import type {
  FlowBrief,
  FlowDetail,
  Frame,
  FrameBatch,
  HARExport,
  ReplayEdit,
  SortKey,
} from '../types'
import { toBrief, toMessage } from './helpers'
import { useConfigStore } from './config'
import { useUiStore } from './ui'

/** Shift 连选的锚点：最近一次单击的行下标 */
let anchorIndex = -1
/** 详情加载令牌，防止快速切换会话时旧响应覆盖新数据 */
let detailToken = 0
let frameToken = 0
/** 已见的最大会话序号。后端 seq 单调递增，重启后归零，用于检测"后端重启、本地残留"。 */
let maxSeqSeen = 0
/** 响应体搜索令牌：快速连续输入时丢弃过期结果 */
let bodySearchToken = 0
let bodySearchTimer: number | undefined

/**
 * 单列比较器。数值列按数值比，文本列按本地化顺序比（带 numeric，
 * "9" < "10"）。JS 的 sort 是稳定排序：相等项保持后端推送的时间序，
 * 所以排序永远不会打乱同一列值内的抓包先后关系。
 */
function compareFlows(a: FlowBrief, b: FlowBrief, key: SortKey): number {
  switch (key) {
    case 'seq':
      return a.seq - b.seq
    case 'status':
      return a.statusCode - b.statusCode
    case 'size':
      return (a.resSize || a.reqSize) - (b.resSize || b.reqSize)
    case 'duration':
      return a.durationMs - b.durationMs
    default: {
      const va = String((a as unknown as Record<string, unknown>)[key] ?? '')
      const vb = String((b as unknown as Record<string, unknown>)[key] ?? '')
      return va.localeCompare(vb, undefined, { numeric: true, sensitivity: 'base' })
    }
  }
}

export const useTrafficStore = defineStore('traffic', {
  state: () => ({
    flows: [] as FlowBrief[],
    selectedId: '',
    /** 多选集合（用于批量删除/导出）。与 selectedId 是两回事：后者决定详情面板显示谁。 */
    selectedIds: [] as string[],
    detail: null as FlowDetail | null,
    detailLoading: false,
    /** 当前详情会话的 WebSocket/SSE 帧 */
    frames: [] as Frame[],
    framesDropped: 0,
    framesLoading: false,
    filter: {
      keyword: '',
      method: '',
      kind: '',
      onlyErrors: false,
      searchBody: false,
    },
    /**
     * 响应体搜索的命中会话 ID 集合（后端扫描解码文本返回）。
     * 是一次快照：之后新到的会话要等下一次搜索才会参与匹配。
     */
    bodyHits: [] as string[],
    /** 列表排序。key 为空表示默认时间序（后端推送顺序）。 */
    sort: { key: '' as '' | SortKey, dir: 'asc' as 'asc' | 'desc' },
  }),

  getters: {
    /** 可筛选的会话列表。过滤只在展示层进行，不丢弃数据。 */
    visibleFlows(state): FlowBrief[] {
      const { keyword, method, kind, onlyErrors, searchBody } = state.filter
      const kw = keyword.trim().toLowerCase()
      // 开了"含响应体"后，关键字同时匹配 URL 与响应体命中集（并集）
      const bodySet =
        searchBody && kw && state.bodyHits.length > 0 ? new Set(state.bodyHits) : null
      const list = state.flows.filter((f) => {
        if (onlyErrors && !f.error && f.statusCode < 400) return false
        if (method && f.method !== method) return false
        if (kind && f.kind !== kind) return false
        if (kw) {
          const hay = `${f.host}${f.path}${f.query}`.toLowerCase()
          if (!hay.includes(kw) && !(bodySet && bodySet.has(f.id))) return false
        }
        return true
      })
      if (state.sort.key) {
        const key = state.sort.key
        const dir = state.sort.dir === 'desc' ? -1 : 1
        return list.sort((a, b) => compareFlows(a, b, key) * dir)
      }
      return list
    },

    /** 多选集合的 O(1) 查询视图。 */
    selectedSet(state): Set<string> {
      return new Set(state.selectedIds)
    },

    /** 选中项中仍然存在的会话（删除/淘汰后自动失联的 ID 不应影响导出条数）。 */
    selectedFlows(state): FlowBrief[] {
      const set = new Set(state.selectedIds)
      return state.flows.filter((f) => set.has(f.id))
    },
  },

  actions: {
    /* ---------------- 事件入口（app.ts 的 bindEvents 调用） ---------------- */

    /** 后端推送的会话批次：新增 / 原位更新 / 重启对账。 */
    applyFlowEvents(batch: FlowBrief[] | null): void {
      if (!Array.isArray(batch) || batch.length === 0) return
      const index = new Map(this.flows.map((f, i) => [f.id, i]))
      for (const raw of batch) {
        const brief = toBrief(raw)
        const at = index.get(brief.id)
        if (at === undefined) {
          // seq 在后端进程内单调递增；若新会话的 seq 不比已见最大值大，
          // 说明后端重启过（seq 从头计数），本地残留的旧会话已不存在。
          // 全量对账一次，避免幽灵会话和重复序号长期留在界面上。
          if (maxSeqSeen > 0 && brief.seq <= maxSeqSeen) {
            void this.resyncFlows()
            return
          }
          this.flows.push(brief)
        } else {
          // 同一会话内容更新时原位替换，保持时间序稳定
          this.flows[at] = brief
        }
        if (brief.seq > maxSeqSeen) maxSeqSeen = brief.seq
      }
      this.syncProxyFlowCount()
    },

    /** 后端推送的帧增量。 */
    applyFrameEvents(batch: FrameBatch | null): void {
      if (!batch || !Array.isArray(batch.frames)) return
      // 帧计数同步到列表，让用户知道这条长连接"还活着"
      const brief = this.flows.find((f) => f.id === batch.flowId)
      if (brief) brief.frames = batch.total
      if (batch.flowId !== this.selectedId) return

      // 后端按顺序追加且 index 单调递增，因此只接收尚未见过的部分，
      // 这样"先拉快照、后收增量"不会出现重复帧
      const known = this.frames.length
      for (const frame of batch.frames) {
        if (frame.index >= known) this.frames.push(frame)
      }
      this.framesDropped = batch.dropped
    },

    /** 会话被清空（清空按钮或多端同步）。 */
    resetFlows(): void {
      this.flows = []
      this.selectedId = ''
      this.selectedIds = []
      this.detail = null
      this.frames = []
      this.framesDropped = 0
      this.bodyHits = []
      anchorIndex = -1
    },

    /** 会话被删除。 */
    removeFlows(ids: string[]): void {
      if (ids.length === 0) return
      const gone = new Set(ids)
      this.flows = this.flows.filter((f) => !gone.has(f.id))
      this.selectedIds = this.selectedIds.filter((id) => !gone.has(id))
      if (gone.has(this.selectedId)) this.closeDetail()
      this.syncProxyFlowCount()
    },

    /** 用后端列表整体替换本地会话（初始化与重启对账共用）。 */
    replaceFlows(flows: FlowBrief[] | null): void {
      this.flows = (flows ?? []).map(toBrief)
      maxSeqSeen = this.flows.reduce((m, f) => Math.max(m, f.seq), 0)
      this.syncProxyFlowCount()
    },

    /** 让顶栏徽标与列表长度一致（proxy.flows 由 config 域持有）。 */
    syncProxyFlowCount(): void {
      useConfigStore().proxy.flows = this.flows.length
    },

    /**
     * 与后端全量对账：以后端列表为准整体替换本地会话。
     * 触发时机是检测到后端进程重启（seq 归零），
     * 一次性丢弃本地残留的"幽灵会话"，选中态按还存在的会话保留。
     */
    async resyncFlows(): Promise<void> {
      try {
        const flows = (await Backend.ListFlows(0)) as unknown as FlowBrief[] | null
        this.replaceFlows(flows)
        const alive = new Set(this.flows.map((f) => f.id))
        this.selectedIds = this.selectedIds.filter((id) => alive.has(id))
        if (this.selectedId && !alive.has(this.selectedId)) this.closeDetail()
      } catch {
        // 对账失败不打断抓包；下一批事件若仍异常会再次触发
      }
    },

    /* ---------------- 详情 ---------------- */

    /** 选中一条会话并加载详情（含长连接的帧）。 */
    async selectFlow(id: string): Promise<void> {
      this.selectedId = id
      this.detailLoading = true
      this.framesLoading = true
      this.detail = null
      this.frames = []
      this.framesDropped = 0

      const token = ++detailToken
      const ftoken = ++frameToken
      try {
        const detail = (await Backend.FlowDetail(id)) as FlowDetail | null
        if (token !== detailToken) return // 已切换到别的会话，丢弃过期响应
        this.detail = detail
      } catch (err) {
        if (token === detailToken) {
          this.detail = null
          useUiStore().showToast(toMessage(err), 'error')
        }
      } finally {
        if (token === detailToken) this.detailLoading = false
      }

      try {
        const batch = (await Backend.Frames(id)) as unknown as FrameBatch
        if (ftoken !== frameToken) return
        this.frames = batch?.frames ?? []
        this.framesDropped = batch?.dropped ?? 0
      } catch {
        // 帧是附加信息，拉取失败不应打断详情展示
      } finally {
        if (ftoken === frameToken) this.framesLoading = false
      }
    },

    /** 关闭详情面板。 */
    closeDetail(): void {
      this.selectedId = ''
      this.detail = null
      this.frames = []
      this.framesDropped = 0
    },

    /* ---------------- 过滤与排序 ---------------- */

    /**
     * 响应体搜索（内部防抖 250ms，避免每个按键都打一次后端）。
     * 关键字为空或未开启"含响应体"时直接清空命中集；
     * 后端扫描失败时静默降级为仅 URL 过滤，不打断使用。
     */
    searchBodies(): void {
      window.clearTimeout(bodySearchTimer)
      const kw = this.filter.keyword.trim()
      if (!this.filter.searchBody || kw === '') {
        this.bodyHits = []
        return
      }
      bodySearchTimer = window.setTimeout(async () => {
        const token = ++bodySearchToken
        try {
          const hits = (await Backend.SearchBodies(kw)) as unknown as FlowBrief[] | null
          if (token !== bodySearchToken) return
          this.bodyHits = (hits ?? []).map((f) => f.id)
        } catch {
          if (token === bodySearchToken) this.bodyHits = []
        }
      }, 250)
    },

    /**
     * 点击表头排序：新列从升序开始，同列在 升 → 降 → 时间序 之间循环。
     * 排序只在展示层生效，不影响后端推送顺序与选中状态。
     */
    setSort(key: SortKey): void {
      if (this.sort.key !== key) {
        this.sort = { key, dir: 'asc' }
        return
      }
      if (this.sort.dir === 'asc') this.sort = { key, dir: 'desc' }
      else this.sort = { key: '', dir: 'asc' }
    },

    /* ---------------- 批量操作 ---------------- */

    /** 清空会话。会话是调试期间产物，清空后不可恢复，所以前端二次确认。 */
    async clearFlows(): Promise<void> {
      if (this.flows.length === 0) return
      const ui = useUiStore()
      const { t } = useI18n()
      const ok = await ui.confirmAction({
        title: t('清空会话'),
        message: t('确认清空 {count} 条会话？此操作不可恢复。', { count: this.flows.length }),
        confirmText: t('清空'),
        danger: true,
      })
      if (!ok) return
      try {
        await Backend.ClearFlows()
        this.resetFlows()
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
      }
    },

    /** 删除指定会话（单条或多选）。 */
    async deleteFlows(ids: string[]): Promise<void> {
      if (ids.length === 0) return
      const ui = useUiStore()
      const { t } = useI18n()
      const ok = await ui.confirmAction({
        title: t('删除会话'),
        message: t('确认删除选中的 {count} 条会话？', { count: ids.length }),
        confirmText: t('删除'),
        danger: true,
      })
      if (!ok) return
      try {
        const n = (await Backend.DeleteFlows(ids)) as number
        // 后端同时会推送 flows-deleted 事件；这里先本地剔除，避免等事件时界面"卡一下"
        this.removeFlows(ids)
        ui.showToast(t('已删除 {count} 条会话', { count: n }), 'success')
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
      }
    },

    /* ---------------- 选择 ---------------- */

    /** 切换某一行的多选状态；shift 为 true 时按上次锚点做区间选择。 */
    toggleSelect(id: string, index: number, shift = false): void {
      const list = this.visibleFlows
      if (shift && anchorIndex >= 0 && anchorIndex < list.length) {
        const from = Math.min(anchorIndex, index)
        const to = Math.max(anchorIndex, index)
        const add = list.slice(from, to + 1).map((f) => f.id)
        const set = new Set(this.selectedIds)
        for (const item of add) set.add(item)
        this.selectedIds = [...set]
        return
      }

      const set = new Set(this.selectedIds)
      if (set.has(id)) set.delete(id)
      else set.add(id)
      this.selectedIds = [...set]
      anchorIndex = index
    },

    /** 全选/取消全选当前可见项（过滤后）。 */
    toggleSelectAll(): void {
      const list = this.visibleFlows
      if (list.length === 0) return
      if (this.selectedIds.length >= list.length) {
        this.selectedIds = []
        return
      }
      this.selectedIds = list.map((f) => f.id)
      anchorIndex = list.length - 1
    },

    /** 分组视图里整组勾选：整组都已选中则撤销，否则全部补上。 */
    toggleGroupSelect(ids: string[]): void {
      if (ids.length === 0) return
      const set = new Set(this.selectedIds)
      const allSelected = ids.every((id) => set.has(id))
      for (const id of ids) {
        if (allSelected) set.delete(id)
        else set.add(id)
      }
      this.selectedIds = [...set]
    },

    /** 清空多选。 */
    clearSelection(): void {
      this.selectedIds = []
      anchorIndex = -1
    },

    /* ---------------- 导出与重放 ---------------- */

    /**
     * 让用户挑保存位置。
     * 返回 '' 表示用户取消（调用方静默退出）；返回 null 表示对话框本身不可用，
     * 此时退回后端的默认下载目录，避免导出能力因为一次对话框故障整体不可用。
     */
    async pickExportPath(ext: string, label: string): Promise<string | null> {
      const d = new Date()
      const p = (n: number): string => String(n).padStart(2, '0')
      const name = `flowlens-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
      const ui = useUiStore()
      const { t } = useI18n()
      try {
        return await Dialogs.SaveFile({
          Title: t('导出 {label}', { label }),
          Filename: `${name}.${ext}`,
          CanCreateDirectories: true,
          CanSelectHiddenExtension: true,
          Filters: [{ DisplayName: label, Pattern: `*.${ext}` }],
        })
      } catch (err) {
        ui.showToast(
          t('打开保存对话框失败，改存到下载目录：{message}', { message: toMessage(err) }),
          'error',
        )
        return null
      }
    },

    /** 导出 HAR。选中的条目优先，否则导出全部。保存位置由用户在系统对话框里选择。 */
    async exportHAR(ids: string[] = []): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      const path = await this.pickExportPath('har', 'HAR')
      if (path === '') return
      try {
        const res = (await Backend.ExportHAR(ids, path ?? '')) as unknown as HARExport
        ui.showToast(t('已导出 {count} 条到 {path}', { count: res.entries, path: res.path }), 'success')
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
      }
    },

    /** 导出 观流 JSON 快照（与 HAR 互补：给脚本加工与回归对比用）。 */
    async exportJSON(ids: string[] = []): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      const path = await this.pickExportPath('json', '观流 JSON')
      if (path === '') return
      try {
        const res = (await Backend.ExportJSON(ids, path ?? '')) as unknown as HARExport
        ui.showToast(t('已导出 {count} 条到 {path}', { count: res.entries, path: res.path }), 'success')
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
      }
    },

    /** 重放指定会话（原样重发），成功后自动打开新产生的会话。 */
    async replayFlow(id: string): Promise<void> {
      try {
        await this.replayFlowEdit(id, null)
      } catch (err) {
        useUiStore().showToast(toMessage(err), 'error')
      }
    },

    /**
     * 重放（可按编辑后的内容）。edit 为 null 表示原样重发。
     * 返回新的会话详情，供弹窗展示本次响应；失败抛给调用方，由弹窗就地提示。
     */
    async replayFlowEdit(id: string, edit: ReplayEdit | null): Promise<FlowDetail> {
      const { t } = useI18n()
      const detail =
        edit === null
          ? ((await Backend.ReplayFlow(id)) as unknown as FlowDetail)
          : ((await Backend.ReplayFlowEdit(id, edit)) as unknown as FlowDetail)
      // 新会话由后端通过事件推送到列表；这里稍等一拍再选中，避免拿到空详情
      if (detail?.id) await this.selectFlow(detail.id)
      useUiStore().showToast(edit === null ? t('重放完成') : t('已按编辑内容发送'), 'success')
      return detail
    },
  },
})
