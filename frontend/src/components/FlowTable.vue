<script setup lang="ts">
/**
 * 会话列表：固定行高 + 可视窗口渲染（虚拟滚动）。
 *
 * 为什么必须虚拟滚动：抓包场景动辄几千条，全量渲染 DOM 会让界面直接失去响应。
 * 这里只渲染"可视区域 + 上下各 6 行缓冲"，DOM 数量恒定在 30 行左右。
 *
 * 多选交互：行首复选框做精确勾选，行体支持 Ctrl/Cmd 追加、Shift 连选，
 * 与 Chrome DevTools 的 Network 面板保持一致，降低学习成本。
 *
 * 分组模式：按域名聚合，组头也是列表里的一行（与数据行同高），
 * 因此折叠/展开不需要第二套滚动逻辑，行高统一也就不会算错滚动位置。
 */
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from '../i18n'
import { useUiStore } from '../stores/ui'
import type { FlowBrief } from '../types'
import { displayHost, flowURL, formatDuration, formatSize, methodClass, statusClass } from '../format'

const { t } = useI18n()
const ui = useUiStore()

const props = defineProps<{
  flows: FlowBrief[]
  selectedId: string
  selectedIds: string[]
  /** 按域名分组展示 */
  grouped?: boolean
  /** 当前排序。key 为空表示默认时间序。 */
  sort?: { key: string; dir: 'asc' | 'desc' }
}>()

const emit = defineEmits<{
  (e: 'select', id: string): void
  (e: 'toggle', id: string, index: number, shift: boolean): void
  (e: 'toggle-all'): void
  (e: 'toggle-group', ids: string[]): void
  (e: 'sort', key: string): void
}>()

/**
 * 行高取自 CSS 变量（--fl-row-h），大屏风格会放大到 40px。
 * 显式依赖 ui.style 触发重算：虚拟滚动的像素定位必须与 CSS 严格一致，
 * 否则切换风格后行会错位。
 */
const rowHeight = computed(() => {
  void ui.style
  const raw = getComputedStyle(document.documentElement).getPropertyValue('--fl-row-h')
  return Number.parseInt(raw, 10) || 32
})
const BUFFER = 6

const bodyRef = ref<HTMLElement | null>(null)
const headCheckRef = ref<HTMLInputElement | null>(null)
const scrollTop = ref(0)
const viewportHeight = ref(400)
const pinnedToBottom = ref(true)
/**
 * 是否处于窄视图（详情面板展开后列表会被挤窄）。
 * 用 ResizeObserver 实测宽度而不是 CSS 容器查询：后者在 WebKit/旧内核里
 * 对 flex 子项的表现不稳定，实测宽度更可控，也便于断点调试。
 */
const compact = ref(false)
const COMPACT_WIDTH = 620

let observer: ResizeObserver | null = null

const totalHeight = computed(() => rows.value.length * rowHeight.value)

const startIndex = computed(() => {
  const first = Math.floor(scrollTop.value / rowHeight.value) - BUFFER
  return Math.max(0, first)
})

const endIndex = computed(() => {
  const visible = Math.ceil(viewportHeight.value / rowHeight.value) + BUFFER * 2
  return Math.min(rows.value.length, startIndex.value + visible)
})

const visibleRows = computed(() => rows.value.slice(startIndex.value, endIndex.value))

const selectedSet = computed(() => new Set(props.selectedIds))
const allSelected = computed(
  () => props.flows.length > 0 && props.selectedIds.length >= props.flows.length,
)
const someSelected = computed(
  () => props.selectedIds.length > 0 && !allSelected.value,
)

/** 分组模式下被折叠的域名。用 Set 而非对象，判断与切换都更直白。 */
const collapsed = ref<Set<string>>(new Set())

/**
 * 新到会话的短暂高亮。用 seq 增量识别，不用整表 diff（5000 条时太重）。
 * 1.2s 后清空集合，之后虚拟滚动重新挂载行也不会再闪——避免"滚动时全场闪烁"。
 */
const freshIds = ref<Set<string>>(new Set())
let lastSeq = 0
let freshTimer: number | undefined
let hasInitialBatch = false
watch(
  () => props.flows,
  (list) => {
    const max = list.reduce((m, f) => Math.max(m, Math.max(0, f.seq)), lastSeq)
    if (!hasInitialBatch) {
      // 首屏本来就有几百条时不能全闪一遍
      hasInitialBatch = true
      lastSeq = max
      return
    }
    const incoming = list.filter((f) => f.seq > lastSeq).map((f) => f.id)
    if (incoming.length > 0) {
      const next = new Set(freshIds.value)
      for (const id of incoming) next.add(id)
      freshIds.value = next
      window.clearTimeout(freshTimer)
      freshTimer = window.setTimeout(() => {
        freshIds.value = new Set()
      }, 1200)
    }
    lastSeq = max
  },
  { immediate: true },
)
onUnmounted(() => window.clearTimeout(freshTimer))

interface Group {
  host: string
  flows: FlowBrief[]
}

/**
 * 按域名聚合。顺序沿用首次出现的时间序：
 * 采集过程中新域名只会出现在末尾，不会让已有分组跳位置而打断用户视线。
 */
const groups = computed<Group[]>(() => {
  const map = new Map<string, FlowBrief[]>()
  for (const f of props.flows) {
    const key = f.host || t('(未知主机)')
    const bucket = map.get(key)
    if (bucket) bucket.push(f)
    else map.set(key, [f])
  }
  return [...map.entries()].map(([host, flows]) => ({ host, flows }))
})

type Row =
  | { kind: 'group'; group: Group; collapsed: boolean }
  | { kind: 'flow'; flow: FlowBrief; index: number; seq: number }

/** 展平成"一行一个条目"，虚拟滚动只需要处理一维数组。 */
const rows = computed<Row[]>(() => {
  if (!props.grouped) {
    return props.flows.map((flow, index) => ({ kind: 'flow', flow, index, seq: index + 1 }))
  }
  const order = new Map(props.flows.map((f, i) => [f.id, i]))
  const out: Row[] = []
  for (const group of groups.value) {
    const folded = collapsed.value.has(group.host)
    out.push({ kind: 'group', group, collapsed: folded })
    if (folded) continue
    for (const flow of group.flows) {
      // seq 用后端编号而不是显示下标：分组后下标没有意义，编号仍能对上时间序
      out.push({ kind: 'flow', flow, index: order.get(flow.id) ?? 0, seq: flow.seq })
    }
  }
  return out
})

/** 组头复选框：整组都已选中才算选中。 */
function groupChecked(g: Group): boolean {
  const set = selectedSet.value
  return g.flows.every((f) => set.has(f.id))
}

function groupErrorCount(g: Group): number {
  return g.flows.filter((f) => f.error || f.statusCode >= 400).length
}

function toggleCollapse(host: string): void {
  const next = new Set(collapsed.value)
  if (next.has(host)) next.delete(host)
  else next.add(host)
  collapsed.value = next
}

/** 切换列表形态时清空折叠状态：用户回来时应当看到全部内容，而不是上次折叠的残留。 */
watch(
  () => props.grouped,
  () => {
    collapsed.value = new Set()
    if (bodyRef.value) bodyRef.value.scrollTop = 0
    scrollTop.value = 0
  },
)

/** 排序变化时回到列表顶部：排序结果的第一行才是用户想看的。 */
watch(
  () => props.sort,
  () => {
    if (bodyRef.value) bodyRef.value.scrollTop = 0
    scrollTop.value = 0
    pinnedToBottom.value = !props.sort?.key
  },
)

/** 当前列的排序指示符：未排序列不显示，避免一排箭头制造噪音。 */
function sortMark(key: string): string {
  if (props.sort?.key !== key) return ''
  return props.sort.dir === 'asc' ? '↑' : '↓'
}

// 半选状态只能通过 DOM 属性设置，模板里没有对应绑定
watch([someSelected, allSelected], () => {
  if (headCheckRef.value) headCheckRef.value.indeterminate = someSelected.value
})

function onScroll(): void {
  const el = bodyRef.value
  if (!el) return
  scrollTop.value = el.scrollTop
  // 距底部 24px 内视为"跟随最新"，超过则说明用户正在回看历史，不应强行拽回底部
  pinnedToBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 24
}

/** 新会话到达时，只有用户本来就在底部且未排序（默认时间序）才自动跟随。 */
watch(
  () => props.flows.length,
  () => {
    if (!pinnedToBottom.value || props.sort?.key) return
    requestAnimationFrame(() => {
      const el = bodyRef.value
      if (el) el.scrollTop = el.scrollHeight
    })
  },
)

onMounted(() => {
  const el = bodyRef.value
  if (!el) return
  observer = new ResizeObserver(() => {
    viewportHeight.value = el.clientHeight
    compact.value = el.clientWidth < COMPACT_WIDTH
  })
  observer.observe(el)
  viewportHeight.value = el.clientHeight
  compact.value = el.clientWidth < COMPACT_WIDTH
})

onUnmounted(() => {
  observer?.disconnect()
})

/** 行点击：普通点击看详情，Ctrl/Cmd 追加多选，Shift 连选。 */
function onRowClick(row: FlowBrief, index: number, ev: MouseEvent): void {
  if (ev.metaKey || ev.ctrlKey || ev.shiftKey) {
    emit('toggle', row.id, index, ev.shiftKey)
    return
  }
  emit('select', row.id)
}

/** 复选框点击：精确勾选，不改变详情面板。 */
function onCheck(index: number, ev: MouseEvent): void {
  const row = props.flows[index]
  if (!row) return
  emit('toggle', row.id, index, ev.shiftKey)
}

function onGroupCheck(g: Group, ev: MouseEvent): void {
  ev.stopPropagation()
  emit(
    'toggle-group',
    g.flows.map((f) => f.id),
  )
}

function title(row: FlowBrief): string {
  return `${row.method} ${flowURL(row)}`
}
</script>

<template>
  <div class="table" :class="{ compact, grouped }">
    <div class="head">
      <span class="c-check">
        <input
          ref="headCheckRef"
          type="checkbox"
          :checked="allSelected"
          :disabled="flows.length === 0"
          :title="t('全选/取消全选当前筛选结果')"
          @change="emit('toggle-all')"
        />
      </span>
      <span class="c-idx sortable" :title="t('按序号排序')" @click="emit('sort', 'seq')">
        #<i v-if="sortMark('seq')" class="mark">{{ sortMark('seq') }}</i>
      </span>
      <span class="c-method sortable" :title="t('按方法排序')" @click="emit('sort', 'method')">
        {{ t('方法') }}<i v-if="sortMark('method')" class="mark">{{ sortMark('method') }}</i>
      </span>
      <span class="c-host sortable" :title="t('按主机排序')" @click="emit('sort', 'host')">
        {{ t('主机') }}<i v-if="sortMark('host')" class="mark">{{ sortMark('host') }}</i>
      </span>
      <span class="c-path sortable" :title="t('按路径排序')" @click="emit('sort', 'path')">
        {{ t('路径') }}<i v-if="sortMark('path')" class="mark">{{ sortMark('path') }}</i>
      </span>
      <span class="c-status num sortable" :title="t('按状态码排序')" @click="emit('sort', 'status')">
        {{ t('状态') }}<i v-if="sortMark('status')" class="mark">{{ sortMark('status') }}</i>
      </span>
      <span class="c-kind sortable" :title="t('按类型排序')" @click="emit('sort', 'kind')">
        {{ t('类型') }}<i v-if="sortMark('kind')" class="mark">{{ sortMark('kind') }}</i>
      </span>
      <span class="c-size num sortable" :title="t('按大小排序')" @click="emit('sort', 'size')">
        {{ t('大小') }}<i v-if="sortMark('size')" class="mark">{{ sortMark('size') }}</i>
      </span>
      <span class="c-dur num sortable" :title="t('按耗时排序')" @click="emit('sort', 'duration')">
        {{ t('耗时') }}<i v-if="sortMark('duration')" class="mark">{{ sortMark('duration') }}</i>
      </span>
    </div>

    <div ref="bodyRef" class="body" @scroll="onScroll">
      <div v-if="flows.length === 0" class="empty">
        <p class="empty-title">{{ t('还没有流量') }}</p>
        <p class="muted">
          {{ t('启动代理后，把客户端 HTTP 代理指向') }} <code>{{ t('本机地址') }}</code>{{ t('，请求就会出现在这里。') }}
        </p>
      </div>

      <div v-else class="spacer" :style="{ height: totalHeight + 'px' }">
        <template v-for="(row, i) in visibleRows" :key="row.kind === 'group' ? 'g:' + row.group.host : row.flow.id">
          <!-- 组头：点整行折叠/展开，复选框整组勾选 -->
          <div
            v-if="row.kind === 'group'"
            class="row group"
            :class="{ folded: row.collapsed }"
            :style="{ transform: `translateY(${(startIndex + i) * rowHeight}px)` }"
            @click="toggleCollapse(row.group.host)"
          >
            <span class="c-check">
              <input
                type="checkbox"
                :checked="groupChecked(row.group)"
                :disabled="row.group.flows.length === 0"
                :aria-label="t('选择 {host} 的全部请求', { host: row.group.host })"
                :title="t('选择 {host} 的全部 {count} 条', { host: row.group.host, count: row.group.flows.length })"
                @click="onGroupCheck(row.group, $event)"
              />
            </span>
            <span class="arrow">{{ row.collapsed ? '▸' : '▾' }}</span>
            <span class="g-host">{{ row.group.host }}</span>
            <span class="g-count muted">{{ t('{count} 条', { count: row.group.flows.length }) }}</span>
            <span v-if="groupErrorCount(row.group) > 0" class="g-err">
              {{ t('{count} 异常', { count: groupErrorCount(row.group) }) }}
            </span>
          </div>

          <div
            v-else
            class="row"
            :class="{
              selected: row.flow.id === selectedId,
              checked: selectedSet.has(row.flow.id),
              failed: !!row.flow.error,
              fresh: freshIds.has(row.flow.id),
            }"
            :style="{ transform: `translateY(${(startIndex + i) * rowHeight}px)` }"
            :title="title(row.flow)"
            @click="onRowClick(row.flow, row.index, $event)"
          >
            <span class="c-check">
              <input
                type="checkbox"
                :checked="selectedSet.has(row.flow.id)"
                :aria-label="t('选择第 {seq} 条', { seq: row.seq })"
                @click.stop="onCheck(row.index, $event)"
              />
            </span>
            <span class="c-idx muted">{{ row.seq }}</span>
            <span class="method c-method" :class="methodClass(row.flow.method)">
              {{ row.flow.method }}
              <span v-if="row.flow.replayed" class="replay" :title="t('该请求由重放产生')">↻</span>
              <span
                v-if="row.flow.mapped"
                class="mapped"
                :title="t('响应已由本地文件替换：{path}', { path: row.flow.mapped })"
              >
                ⌂
              </span>
              <span
                v-if="row.flow.remoted"
                class="remoted"
                :title="t('请求已改写到：{target}', { target: row.flow.remoted })"
              >
                ⇄
              </span>
            </span>
            <span class="host c-host">{{ displayHost(row.flow) }}</span>
            <span class="path c-path">
              <span v-if="row.flow.scheme === 'https'" class="lock" :title="t('HTTPS 已解密')">🔒</span>
              <span v-else class="lock-off" :title="t('未加密的 HTTP')">·</span>
              {{ row.flow.path || '/' }}
              <span v-if="row.flow.query" class="query">?{{ row.flow.query }}</span>
            </span>
            <span class="num status c-status" :class="statusClass(row.flow.statusCode)">
              {{ row.flow.statusCode || (row.flow.error ? 'ERR' : '…') }}
            </span>
            <span class="kind c-kind">
              {{ row.flow.kind }}
              <span v-if="row.flow.frames > 0" class="frames">· {{ row.flow.frames }}</span>
            </span>
            <span class="num c-size">{{ formatSize(row.flow.resSize || row.flow.reqSize, '-') }}</span>
            <span class="num c-dur">{{ formatDuration(row.flow.durationMs) }}</span>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<style scoped>
.table {
  display: flex;
  flex-direction: column;
  min-height: 0;
  width: 100%;
  height: 100%;
  overflow: hidden;
}

.head,
.row {
  display: grid;
  /* 用 minmax(0, …) 而非固定最小宽度：长 URL 不会把表格撑出容器导致横向溢出 */
  grid-template-columns: 28px 34px 62px minmax(0, 1fr) minmax(0, 1.8fr) 54px 58px 64px 60px;
  align-items: center;
  gap: 8px;
  padding: 0 12px;
}

/* 列表被详情面板挤窄时，只保留"方法/主机/路径/状态"四列关键信息 */
.table.compact .head,
.table.compact .row {
  grid-template-columns: 24px 30px 54px minmax(0, 1fr) minmax(0, 1.5fr) 50px;
}

.table.compact .c-kind,
.table.compact .c-size,
.table.compact .c-dur {
  display: none;
}

/* 分组视图里主机名已经在组头上，再占一列纯属浪费横向空间 */
.table.grouped:not(.compact) .head,
.table.grouped:not(.compact) .row:not(.group) {
  grid-template-columns: 28px 34px 62px minmax(0, 1fr) 54px 58px 64px 60px;
}

.table.grouped.compact .head,
.table.grouped.compact .row:not(.group) {
  grid-template-columns: 24px 30px 54px minmax(0, 1fr) 50px;
}

.table.grouped .c-host {
  display: none;
}

.head {
  height: var(--fl-head-h);
  white-space: nowrap;
  font-size: 11px;
  color: var(--fl-muted);
  border-bottom: 1px solid var(--fl-border);
  background: var(--fl-panel-2);
  flex: none;
}

/* 可点击排序的表头：hover 提示可交互，指示符跟在列名后面 */
.head .sortable {
  cursor: pointer;
  user-select: none;
}

.head .sortable:hover {
  color: var(--fl-text);
}

.head .mark {
  font-style: normal;
  margin-left: 1px;
  color: var(--fl-accent);
  font-size: 10px;
  animation: fl-pop 0.24s var(--fl-ease) both;
}

/* 新会话落表时闪一下强调色，1.2s 后由脚本移除 class */
.row.fresh {
  animation: fl-fresh 1.1s ease-out both;
}

.body {
  position: relative;
  overflow-y: auto;
  flex: 1;
  min-height: 0;
}

.spacer {
  position: relative;
  width: 100%;
}

.row {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  height: var(--fl-row-h);
  border-bottom: 1px solid var(--fl-border);
  cursor: default;
  white-space: nowrap;
  overflow: hidden;
}

.row:hover {
  background: var(--fl-panel-2);
}

.row.checked {
  background: var(--fl-accent-soft);
}

.row.selected {
  background: var(--fl-accent-soft);
  box-shadow: inset 2px 0 0 var(--fl-accent);
}

.row.failed .host,
.row.failed .path {
  color: var(--fl-red);
}

/* 组头不参与九列网格：单独用 flex，窄窗口下也不会被压成碎片 */
.row.group {
  display: flex;
  align-items: center;
  gap: 8px;
  background: var(--fl-panel-2);
  font-weight: 600;
  cursor: pointer;
}

.row.group:hover {
  background: var(--fl-accent-soft);
}

.row.group .arrow {
  width: 12px;
  text-align: center;
  font-size: 11px;
  color: var(--fl-text-2);
}

.row.group .g-host {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: var(--fl-mono);
  font-size: 12px;
}

.row.group .g-count,
.row.group .g-err {
  flex: none;
  font-weight: 400;
  font-size: 11px;
}

.row.group .g-err {
  color: var(--fl-red);
}

.c-check {
  display: flex;
  align-items: center;
  justify-content: center;
}

.c-check input {
  width: 13px;
  height: 13px;
  margin: 0;
  cursor: pointer;
  accent-color: var(--fl-accent);
}

.c-idx {
  font-size: 11px;
  text-align: right;
}

.method {
  font-weight: 600;
  font-size: 11px;
  letter-spacing: 0.02em;
  overflow: hidden;
  text-overflow: ellipsis;
}

.replay {
  color: var(--fl-accent);
  margin-left: 2px;
}

/* 本地映射标记：形状与重放的 ↻ 明显不同，扫一眼能区分"发出去的"和"被替换的" */
.mapped {
  color: var(--fl-orange, #d08700);
  margin-left: 2px;
}

/* 远程映射标记：请求被改写发往别处 */
.remoted {
  color: var(--fl-teal, #2a9d8f);
  margin-left: 2px;
}

.host {
  overflow: hidden;
  text-overflow: ellipsis;
}

.path {
  font-family: var(--fl-mono);
  font-size: 12px;
  color: var(--fl-text-2);
  overflow: hidden;
  text-overflow: ellipsis;
}

.query {
  color: var(--fl-muted);
}

.lock {
  font-size: 9px;
  opacity: 0.7;
}

.lock-off {
  opacity: 0;
}

.status {
  font-weight: 600;
  font-family: var(--fl-mono);
  font-size: 12px;
}

.kind {
  font-size: 11px;
  color: var(--fl-muted);
  overflow: hidden;
  text-overflow: ellipsis;
}

.frames {
  color: var(--fl-accent);
}

.num {
  text-align: right;
  font-variant-numeric: tabular-nums;
  font-size: 12px;
}

.empty {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  text-align: center;
  padding: 40px;
}

.empty-title {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}
</style>
