<script setup lang="ts">
/**
 * 流量页：过滤条 + 会话列表 + 详情面板。
 * 过滤只作用于展示，不影响抓取（见 REQUIREMENTS F4-3）。
 */
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { useI18n } from '../i18n'
import FlowTable from './FlowTable.vue'
import FlowDetail from './FlowDetail.vue'
import { useTrafficStore } from '../stores/traffic'
import { useUiStore } from '../stores/ui'
import type { SortKey } from '../types'

const { t } = useI18n()

const traffic = useTrafficStore()
const ui = useUiStore()
const { visibleFlows } = storeToRefs(traffic)

const methods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS', 'HEAD']
// 用 computed 包一层，切换语言后下拉选项里的中文显示名也会跟着更新
const kinds = computed(() => [
  { value: 'xhr', label: 'XHR / JSON' },
  { value: 'doc', label: t('文档') },
  { value: 'js', label: 'JS' },
  { value: 'css', label: 'CSS' },
  { value: 'img', label: t('图片') },
  { value: 'font', label: t('字体') },
  { value: 'media', label: t('音视频') },
  { value: 'ws', label: 'WebSocket' },
  { value: 'sse', label: 'SSE' },
  { value: 'other', label: t('其他') },
])

const total = computed(() => traffic.flows.length)
const shown = computed(() => visibleFlows.value.length)
const selectedCount = computed(() => traffic.selectedIds.length)

/** 关键字、开关或会话数量变化时重新搜索响应体（store 内部已防抖）。 */
watch(
  () => [traffic.filter.keyword, traffic.filter.searchBody, traffic.flows.length],
  () => traffic.searchBodies(),
)

/**
 * 窄窗口适配：左右分栏在可用宽度不足时，详情面板改为浮层覆盖列表。
 * 阈值取「列表最小可读宽度 340 + 面板最小宽度 320」，低于它再分栏会把表格挤到无法阅读
 * （固定列放不下 → 主机/路径列被压缩重叠）。这里用 ResizeObserver 测量而非 CSS 容器查询，
 * 与 FlowTable 的 compact 判定保持一致的做法。
 */
/** 大屏风格下列表与详情都要更宽，窄屏阈值相应提高，避免并排时挤坏表格。 */
function narrowWidth(): number {
  return ui.style === 'spacious' ? 900 : 720
}
const splitRef = ref<HTMLDivElement | null>(null)
const narrow = ref(false)
let splitObserver: ResizeObserver | null = null

/** 有选中项就只导出选中，否则导出全部——按钮文案同步体现，避免歧义。 */
function exportIds(): string[] {
  return selectedCount.value > 0 ? [...traffic.selectedIds] : []
}

function onExport(): void {
  void traffic.exportHAR(exportIds())
}

function onExportJSON(): void {
  void traffic.exportJSON(exportIds())
}

/** 键盘删除：列表上按 Delete/Backspace 删除选中项，符合资源管理器习惯。 */
function onKeydown(ev: KeyboardEvent): void {
  const el = ev.target as HTMLElement | null
  const tag = el?.tagName?.toLowerCase() ?? ''
  // 输入框里的删除键属于文本编辑，不能转义成删除会话
  if (tag === 'input' || tag === 'textarea' || tag === 'select' || el?.isContentEditable) return

  if (ev.key === 'Escape') {
    traffic.clearSelection()
    return
  }
  if ((ev.key === 'Delete' || ev.key === 'Backspace') && selectedCount.value > 0) {
    ev.preventDefault()
    void traffic.deleteFlows([...traffic.selectedIds])
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  const el = splitRef.value
  if (!el) return
  splitObserver = new ResizeObserver((entries) => {
    const w = entries[0]?.contentRect.width ?? el.clientWidth
    narrow.value = w < narrowWidth()
  })
  splitObserver.observe(el)
  narrow.value = el.clientWidth < narrowWidth()
})
watch(
  () => ui.style,
  () => {
    const el = splitRef.value
    if (el) narrow.value = el.clientWidth < narrowWidth()
  },
)

onUnmounted(() => {
  window.removeEventListener('keydown', onKeydown)
  splitObserver?.disconnect()
})
</script>

<template>
  <div class="traffic">
    <div class="filters" :class="{ simple: ui.style === 'simple' }">
      <div v-if="ui.style !== 'simple'" class="seg" role="tablist" :aria-label="t('列表视图')">
        <button
          :class="{ active: ui.listMode === 'list' }"
          :title="t('按时间顺序平铺')"
          @click="ui.listMode = 'list'"
        >
          {{ t('列表') }}
        </button>
        <button
          :class="{ active: ui.listMode === 'group' }"
          :title="t('按域名聚合，可折叠、可整组选择')"
          @click="ui.listMode = 'group'"
        >
          {{ t('分组') }}
        </button>
      </div>
      <input
        v-model="traffic.filter.keyword"
        class="search"
        type="search"
        :placeholder="t('按域名或路径过滤…')"
      />
      <label
        v-if="ui.style !== 'simple'"
        class="check"
        :title="t('关键字同时搜索响应体内容（含 GBK 等已解码文本），URL 与响应体命中取并集')"
      >
        <input v-model="traffic.filter.searchBody" type="checkbox" />
        {{ t('含响应体') }}
      </label>
      <select v-if="ui.style !== 'simple'" v-model="traffic.filter.method">
        <option value="">{{ t('全部方法') }}</option>
        <option v-for="m in methods" :key="m" :value="m">{{ m }}</option>
      </select>
      <select v-if="ui.style !== 'simple'" v-model="traffic.filter.kind">
        <option value="">{{ t('全部类型') }}</option>
        <option v-for="k in kinds" :key="k.value" :value="k.value">{{ k.label }}</option>
      </select>
      <label v-if="ui.style !== 'simple'" class="check">
        <input v-model="traffic.filter.onlyErrors" type="checkbox" />
        {{ t('仅看异常') }}
      </label>
      <div v-if="ui.style !== 'simple'" class="spacer" />
      <span v-if="ui.style !== 'simple'" class="count muted">
        <template v-if="shown !== total">{{ t('显示 {shown} / 共 {total} 条', { shown, total }) }}</template>
        <template v-else>{{ t('共 {total} 条', { total }) }}</template>
      </span>

      <template v-if="selectedCount > 0">
        <span class="selected-tip">{{ t('已选 {selectedCount} 条', { selectedCount }) }}</span>
        <button class="danger" @click="traffic.deleteFlows([...traffic.selectedIds])">{{ t('删除') }}</button>
        <button v-if="ui.style !== 'simple'" @click="traffic.clearSelection">{{ t('取消选择') }}</button>
      </template>

      <button
        v-if="ui.style !== 'simple'"
        :disabled="total === 0"
        :title="
          selectedCount > 0
            ? t('导出选中的会话为 HAR（可被 DevTools / Charles 解析），保存位置自选')
            : t('导出全部会话为 HAR（可被 DevTools / Charles 解析），保存位置自选')
        "
        @click="onExport"
      >
        {{ selectedCount > 0 ? t('导出选中 ({selectedCount})', { selectedCount }) : t('导出 HAR') }}
      </button>
      <button
        v-if="ui.style !== 'simple'"
        class="ghost"
        :disabled="total === 0"
        :title="t('导出为 观流 JSON 快照（保留完整字段，适合脚本加工与回归对比），保存位置自选')"
        @click="onExportJSON"
      >
        JSON
      </button>
      <button v-if="ui.style !== 'simple'" class="ghost" :disabled="total === 0" @click="traffic.clearFlows()">{{ t('清空') }}</button>
    </div>

    <div ref="splitRef" class="split" :class="{ narrow, simple: ui.style === 'simple' }">
      <div class="list">
        <FlowTable
          :flows="visibleFlows"
          :selected-id="traffic.selectedId"
          :selected-ids="traffic.selectedIds"
          :grouped="ui.listMode === 'group'"
          :sort="traffic.sort"
          @select="traffic.selectFlow"
          @toggle="traffic.toggleSelect"
          @toggle-all="traffic.toggleSelectAll"
          @toggle-group="traffic.toggleGroupSelect"
          @sort="traffic.setSort($event as SortKey)"
        />
      </div>
      <div
        v-if="ui.style === 'simple' && traffic.selectedId"
        class="simple-mask"
        @click="traffic.closeDetail()"
      />
      <div v-if="traffic.selectedId" class="panel">
        <FlowDetail :detail="traffic.detail" :loading="traffic.detailLoading" @close="traffic.closeDetail" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.traffic {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  width: 100%;
  overflow: hidden;
}

.filters {
  display: flex;
  align-items: center;
  gap: var(--fl-filter-gap);
  padding: var(--fl-filter-pad-y) var(--fl-filter-pad-x);
  border-bottom: 1px solid var(--fl-border);
  background: var(--fl-panel);
  flex: none;
  white-space: nowrap;
  /* 窗口过窄时换行而不是被裁掉，保证任何尺寸下控件都可达 */
  flex-wrap: wrap;
  row-gap: 6px;
  animation: fl-drop 0.45s var(--fl-ease) 0.04s both;
}

/* 过滤条不允许收缩换行：文本框吸收剩余空间，其余控件保持原宽 */
.filters > * {
  flex: none;
}

.search {
  flex: 1 1 auto;
  min-width: 180px;
  max-width: 320px;
}

.filters select {
  max-width: 130px;
}

.check {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--fl-text-2);
  user-select: none;
}

.spacer {
  flex: 1 1 auto;
  min-width: 0;
}

.count {
  font-size: 12px;
  font-variant-numeric: tabular-nums;
  color: var(--fl-muted);
}

.selected-tip {
  font-size: 12px;
  color: var(--fl-accent);
  font-variant-numeric: tabular-nums;
}

.ghost {
  border-color: var(--fl-border);
  background: transparent;
  color: var(--fl-text-2);
  font-size: 12px;
}

/* 列表 / 分组切换：贴在过滤条最左侧，作为"当前视图"的入口而不是过滤器 */
.seg {
  display: inline-flex;
  border: 1px solid var(--fl-border-strong);
  border-radius: var(--fl-radius-sm);
  overflow: hidden;
}

.seg button {
  border: none;
  border-radius: 0;
  background: transparent;
  color: var(--fl-text-2);
  padding: 4px 10px;
}

.seg button.active {
  background: var(--fl-accent-soft);
  color: var(--fl-accent);
  font-weight: 600;
}

.filters button {
  font-size: 12px;
}

.danger {
  border-color: color-mix(in srgb, var(--fl-red) 45%, transparent);
  color: var(--fl-red);
  background: transparent;
}

.danger:hover {
  background: color-mix(in srgb, var(--fl-red) 12%, transparent);
}

.split {
  flex: 1;
  min-height: 0;
  display: flex;
  overflow: hidden;
  position: relative;
}

.list {
  flex: 1;
  /* 保证表格至少能放下紧凑布局的固定列，不让它在窄窗口下被压成 0 */
  min-width: var(--fl-list-min-w);
  background: var(--fl-panel);
}

.panel {
  width: var(--fl-panel-w);
  flex: none;
  border-left: 1px solid var(--fl-border);
  min-width: 0;
  animation: fl-rise 0.3s var(--fl-ease) both;
}

/* 窄窗口：详情面板浮在列表之上（列表保持全宽可读），关闭按钮回到列表 */
.split.narrow .panel {
  position: absolute;
  inset: 0;
  width: auto;
  z-index: 2;
  border-left: none;
  box-shadow: 0 0 28px rgb(0 0 0 / 0.22);
}

/* 极简风格：搜索框占满整行；详情改为可关闭的右侧浮层 */
.filters.simple .search {
  max-width: none;
}

.simple-mask {
  position: absolute;
  inset: 0;
  z-index: 2;
  background: color-mix(in srgb, var(--fl-text) 20%, transparent);
  backdrop-filter: blur(2px);
  animation: fl-pop 0.2s var(--fl-ease) both;
}

.split.simple .panel {
  position: absolute;
  top: 0;
  right: 0;
  bottom: 0;
  width: min(560px, 92vw);
  z-index: 3;
  border-left: 1px solid var(--fl-border);
  box-shadow: -12px 0 32px rgb(0 0 0 / 0.2);
  animation: fl-pop 0.26s var(--fl-ease) both;
}
</style>
