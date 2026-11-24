<script setup lang="ts">
/**
 * 会话详情面板：概览 / 请求 / 响应 三个页签。
 * body 的格式化与高亮都在前端完成，后端只负责解码与截断。
 */
import { computed, nextTick, ref, watch } from 'vue'
import type { BodyView, FlowDetail, Frame } from '../types'
import { useTrafficStore } from '../stores/traffic'
import { useUiStore } from '../stores/ui'
import { bodyText, displayHost, formatSize, headerRows } from '../format'
import { useI18n } from '../i18n'
import BodyBlock from './BodyBlock.vue'
import BodyPreview from './BodyPreview.vue'
import HeaderTable from './HeaderTable.vue'
import ReplayDialog from './ReplayDialog.vue'

const props = defineProps<{
  detail: FlowDetail | null
  loading: boolean
}>()

const emit = defineEmits<{ (e: 'close'): void }>()

const traffic = useTrafficStore()
const ui = useUiStore()
const { t } = useI18n()

/**
 * 编辑重放弹窗的源会话。
 * 这里保存快照而不是直接用 props.detail：发送成功后详情面板会切到新会话，
 * 若还引用 props，弹窗会突然变成"以刚生成的会话为源"，再点发送就发错东西了。
 */
const replaySource = ref<FlowDetail | null>(null)

/**
 * 页签放在 ui store（detailTab）：排查时经常要连续看多条的"响应"或"预览"，
 * 每次切会话都跳回概览等于逼用户重复点一次，所以跨会话保持用户所在页签。
 * 唯一的例外是「消息」页签：它只在有帧的会话上存在，落到普通请求上时回概览。
 */
watch(
  () => props.detail?.id,
  () => {
    if (ui.detailTab === 'frames' && !hasFrames.value) ui.detailTab = 'overview'
  },
)

/** 有帧才显示「消息」页签：普通请求不该多出一个永远空的标签。 */
const hasFrames = computed(() => {
  const d = props.detail
  if (!d) return false
  return d.frames > 0 || d.kind === 'ws' || d.kind === 'sse'
})

const framesBody = ref<HTMLElement | null>(null)
let framesPinned = true

/** 新帧到达时，若用户没有往上翻就自动跟随到底部。 */
watch(
  () => traffic.frames.length,
  () => {
    if (!framesPinned || ui.detailTab !== 'frames') return
    void nextTick(() => {
      const el = framesBody.value
      if (el) el.scrollTop = el.scrollHeight
    })
  },
)

function onFramesScroll(): void {
  const el = framesBody.value
  if (!el) return
  framesPinned = el.scrollHeight - el.scrollTop - el.clientHeight < 24
}

/** 帧内容展示：二进制没有内联内容时只报告大小。 */
function frameText(f: Frame): string {
  if (f.kind === 'binary') {
    return f.base64 ? t('[二进制 {size} 字节，已内联]', { size: f.size }) : t('[二进制 {size} 字节]', { size: f.size })
  }
  return f.text || ''
}

/** 帧相对会话开始的时间偏移，便于观察时序。 */
function frameOffset(f: Frame): string {
  const start = props.detail?.startTime
  if (!start) return ''
  const delta = new Date(f.timestamp).getTime() - new Date(start).getTime()
  if (!isFinite(delta) || delta < 0) return ''
  return delta < 1000 ? `+${delta}ms` : `+${(delta / 1000).toFixed(2)}s`
}

/** 非 UTF-8 时在正文标题上提示已转码，避免用户误以为服务端发的就是 UTF-8。 */
function encodingHint(body?: BodyView): string {
  if (!body || !body.encoding) return ''
  return body.encoding === 'utf-8' ? '' : t('原字符集 {encoding}，已转 UTF-8 显示', { encoding: body.encoding })
}

const fullURL = computed(() => {
  const d = props.detail
  if (!d) return ''
  // 端口规则与后端 capture.FlowBrief.URL() 保持一致：
  // 默认端口不显示，非默认端口必须保留，否则 cURL 与 HAR 会指向错误地址
  return `${d.scheme}://${displayHost(d)}${d.path}${d.query ? '?' + d.query : ''}`
})

const reqHeaderRows = computed(() => headerRows(props.detail?.reqHeaders))

/**
 * 预览页签看哪一侧。
 * 默认响应（排查时十次有九次在看返回），响应为空时自动落到请求体，
 * 否则用户点开预览先看到一句"无内容"还得再手动切一次。
 */
const previewSide = ref<'res' | 'req'>('res')

const resHasBody = computed(() => !!props.detail && props.detail.resBody?.kind !== 'empty')
const reqHasBody = computed(() => !!props.detail && props.detail.reqBody?.kind !== 'empty')

function openPreview(): void {
  previewSide.value = resHasBody.value ? 'res' : 'req'
  ui.detailTab = 'preview'
}

const previewBody = computed<BodyView | undefined>(() =>
  previewSide.value === 'req' ? props.detail?.reqBody : props.detail?.resBody,
)

/** 生成可直接执行的 cURL，便于把线上问题搬到本地复现。 */
function toCurl(): string {
  const d = props.detail
  if (!d) return ''
  const parts = [`curl -X ${d.method} '${fullURL.value}'`]
  for (const row of reqHeaderRows.value) {
    parts.push(`  -H '${row.joined.replace(/'/g, `'\\''`)}'`)
  }
  const body = bodyText(d.reqBody)
  if (body) parts.push(`  --data-raw '${body.replace(/'/g, `'\\''`)}'`)
  return parts.join(' \\\n')
}

function formatTime(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return d.toLocaleString('zh-CN', { hour12: false })
}
</script>

<template>
  <div class="detail">
    <div v-if="loading" class="hint">{{ t('加载中…') }}</div>

    <div v-else-if="!detail" class="hint">
      <p>{{ t('选择左侧任意一条会话查看详情') }}</p>
      <p class="muted">{{ t('支持请求头、响应头、Body 与耗时信息') }}</p>
    </div>

    <template v-else>
      <header class="head">
        <div class="line">
          <span class="method">{{ detail.method }}</span>
          <span class="url mono" :title="fullURL">{{ fullURL }}</span>
          <button class="close" :title="t('关闭')" @click="emit('close')">✕</button>
        </div>
        <div class="tabs">
          <button :class="{ active: ui.detailTab === 'overview' }" @click="ui.detailTab = 'overview'">{{ t('概览') }}</button>
          <button :class="{ active: ui.detailTab === 'request' }" @click="ui.detailTab = 'request'">{{ t('请求') }}</button>
          <button :class="{ active: ui.detailTab === 'response' }" @click="ui.detailTab = 'response'">{{ t('响应') }}</button>
          <button :class="{ active: ui.detailTab === 'preview' }" @click="openPreview">{{ t('预览') }}</button>
          <button v-if="hasFrames" :class="{ active: ui.detailTab === 'frames' }" @click="ui.detailTab = 'frames'">
            {{ t('消息') }} <span class="count">{{ detail.frames }}</span>
          </button>
          <div class="spacer" />
          <button class="ghost" :title="t('重新发送一次该请求')" @click="traffic.replayFlow(detail.id)">
            {{ t('重放') }}
          </button>
          <button class="ghost" :title="t('改方法/地址/请求头/请求体后再发送')" @click="replaySource = detail">
            {{ t('编辑重放') }}
          </button>
          <button class="ghost" @click="ui.copyText(toCurl(), t('cURL 已复制'))">{{ t('复制 cURL') }}</button>
        </div>
      </header>

      <div class="content">
        <!-- 概览 -->
        <section v-if="ui.detailTab === 'overview'" class="pane">
          <div v-if="detail.error" class="alert">{{ t('请求失败：{error}', { error: detail.error }) }}</div>
          <dl class="kv">
            <dt>{{ t('地址') }}</dt>
            <dd class="mono break">{{ fullURL }}</dd>
            <dt>{{ t('状态') }}</dt>
            <dd>{{ detail.statusCode || '-' }}<span v-if="detail.error" class="muted">{{ t('（无响应）') }}</span></dd>
            <dt>{{ t('协议') }}</dt>
            <dd>
              {{ detail.proto || '-' }}
              <span v-if="detail.scheme === 'https' && !detail.intercepted" class="muted">
                {{ t('· 未解密（在跳过列表中）') }}
              </span>
            </dd>
            <dt>{{ t('耗时') }}</dt>
            <dd>{{ detail.durationMs }} ms</dd>
            <dt>{{ t('大小') }}</dt>
            <dd>{{ t('请求 {req} · 响应 {res}', { req: formatSize(detail.reqSize), res: formatSize(detail.resSize) }) }}</dd>
            <dt>{{ t('客户端') }}</dt>
            <dd class="mono">{{ detail.clientAddr || '-' }}</dd>
            <dt>{{ t('服务端') }}</dt>
            <dd class="mono">{{ detail.serverAddr || '-' }}</dd>
            <dt>{{ t('时间') }}</dt>
            <dd>{{ formatTime(detail.startTime) }}</dd>
            <dt>{{ t('类型') }}</dt>
            <dd>{{ detail.contentType || '-' }}</dd>
            <dt v-if="detail.mapped">{{ t('本地映射') }}</dt>
            <dd v-if="detail.mapped" class="mono break">
              {{ detail.mapped }}
              <span class="muted">{{ t('（响应已被本地文件替换）') }}</span>
            </dd>
          </dl>
        </section>

        <!-- 请求 -->
        <section v-else-if="ui.detailTab === 'request'" class="pane">
          <h4>{{ t('请求头') }}</h4>
          <HeaderTable :headers="detail.reqHeaders" :empty="t('无请求头')" />

          <h4>
            {{ t('请求体') }}
            <span class="muted small" v-if="detail.reqBody">
              {{ detail.reqBody.kind }} · {{ formatSize(detail.reqBody.size) }}
              <template v-if="detail.reqBody.truncated">{{ t('（已截断）') }}</template>
            </span>
            <span v-if="encodingHint(detail.reqBody)" class="badge">{{ encodingHint(detail.reqBody) }}</span>
            <button
              v-if="detail.reqBody?.preview"
              class="ghost tiny"
              @click="ui.copyText(bodyText(detail.reqBody), t('请求体已复制'))"
            >
              {{ t('复制') }}
            </button>
          </h4>
          <BodyBlock :body="detail.reqBody" />
        </section>

        <!-- 响应 -->
        <section v-else-if="ui.detailTab === 'response'" class="pane">
          <h4>{{ t('响应头') }}</h4>
          <HeaderTable :headers="detail.resHeaders" :empty="t('无响应头')" />

          <h4>
            {{ t('响应体') }}
            <span class="muted small" v-if="detail.resBody">
              {{ detail.resBody.kind }} · {{ formatSize(detail.resBody.size) }}
              <template v-if="detail.resBody.truncated">{{ t('（已截断，仅显示前 {size}）', { size: formatSize(detail.resBody.size) }) }}</template>
            </span>
            <span v-if="encodingHint(detail.resBody)" class="badge">{{ encodingHint(detail.resBody) }}</span>
            <button
              v-if="detail.resBody?.preview"
              class="ghost tiny"
              @click="ui.copyText(bodyText(detail.resBody), t('响应体已复制'))"
            >
              {{ t('复制') }}
            </button>
          </h4>
          <BodyBlock :body="detail.resBody" />
        </section>

        <!-- 预览：格式化后的内容，用来快速扫一眼而不是逐个字符读 -->
        <section v-else-if="ui.detailTab === 'preview'" class="pane">
          <div class="preview-bar">
            <div class="seg" role="tablist" :aria-label="t('预览哪一侧的正文')">
              <button
                :class="{ active: previewSide === 'res' }"
                :disabled="!resHasBody"
                @click="previewSide = 'res'"
              >
                {{ t('响应体') }}
              </button>
              <button
                :class="{ active: previewSide === 'req' }"
                :disabled="!reqHasBody"
                @click="previewSide = 'req'"
              >
                {{ t('请求体') }}
              </button>
            </div>
            <span v-if="previewBody" class="muted small">
              {{ previewBody.kind }} · {{ formatSize(previewBody.size) }}
            </span>
            <button
              v-if="previewBody?.preview"
              class="ghost tiny"
              @click="ui.copyText(bodyText(previewBody), t('正文已复制'))"
            >
              {{ t('复制') }}
            </button>
          </div>
          <BodyPreview :body="previewBody" />
        </section>

        <!-- 长连接消息（WebSocket / SSE） -->
        <section v-else class="pane frames-pane">
          <div v-if="traffic.framesDropped > 0" class="alert">
            {{ t('为保证界面流畅，已丢弃最早的 {count} 条消息（单条连接最多保留 2000 条 / 8MB）。', { count: traffic.framesDropped }) }}
          </div>
          <div v-if="traffic.framesLoading" class="muted small">{{ t('加载中…') }}</div>
          <div v-else-if="traffic.frames.length === 0" class="muted small">
            {{ t('暂无消息。连接建立后收发的每一帧都会实时出现在这里。') }}
          </div>
          <div v-else ref="framesBody" class="frames" @scroll="onFramesScroll">
            <div
              v-for="f in traffic.frames"
              :key="f.index"
              class="frame"
              :class="{ up: f.fromClient, down: !f.fromClient }"
            >
              <div class="frame-head">
                <span class="dir" :title="f.fromClient ? t('客户端 → 服务端') : t('服务端 → 客户端')">
                  {{ f.fromClient ? '↑' : '↓' }}
                </span>
                <span class="idx muted">#{{ f.index + 1 }}</span>
                <span class="kind-tag">{{ f.kind }}</span>
                <span class="muted small">{{ formatSize(f.size) }}</span>
                <span class="muted small offset">{{ frameOffset(f) }}</span>
                <span v-if="f.truncated" class="badge">{{ t('已截断') }}</span>
              </div>
              <pre class="frame-body">{{ frameText(f) }}</pre>
            </div>
          </div>
        </section>
      </div>
    </template>

    <ReplayDialog
      v-if="replaySource"
      :detail="replaySource"
      @close="replaySource = null"
    />
  </div>
</template>


<style scoped>
.detail {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  background: var(--fl-panel);
}

/* 切换页签或会话时正文轻轻浮现，避免内容硬切带来的跳动感 */
.detail section {
  animation: fl-rise 0.26s var(--fl-ease) both;
}

.hint {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 4px;
  color: var(--fl-muted);
}

.head {
  border-bottom: 1px solid var(--fl-border);
  padding: 10px 12px 0;
  flex: none;
}

.line {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}

.method {
  font-weight: 700;
  font-size: 12px;
}

.url {
  flex: 1;
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--fl-text-2);
}

.close {
  border: none;
  background: transparent;
  color: var(--fl-muted);
  padding: 2px 6px;
}

.tabs {
  display: flex;
  align-items: center;
  gap: 2px;
}

.tabs button {
  border: none;
  background: transparent;
  border-bottom: 2px solid transparent;
  border-radius: 0;
  padding: 6px 10px;
  color: var(--fl-text-2);
}

.tabs button.active {
  color: var(--fl-accent);
  border-bottom-color: var(--fl-accent);
  font-weight: 600;
}

.tabs .count {
  font-size: 10px;
  color: var(--fl-muted);
}

.spacer {
  flex: 1;
}

.ghost {
  border: 1px solid var(--fl-border);
  background: transparent;
  color: var(--fl-text-2);
  font-size: 11px;
  padding: 3px 8px;
}

.ghost.tiny {
  padding: 1px 6px;
  margin-left: auto;
}

.content {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
}

.pane {
  padding: 12px 14px 24px;
}

.alert {
  background: var(--fl-red-soft);
  color: var(--fl-red);
  border-radius: var(--fl-radius-sm);
  padding: 8px 10px;
  margin-bottom: 12px;
  font-size: 12px;
}

.kv {
  display: grid;
  grid-template-columns: 72px 1fr;
  gap: 8px 12px;
  margin: 0;
}

.kv dt {
  color: var(--fl-muted);
}

.kv dd {
  margin: 0;
}

.break {
  word-break: break-all;
}

h4 {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 16px 0 6px;
  font-size: 12px;
  font-weight: 600;
  color: var(--fl-text-2);
}

h4:first-child {
  margin-top: 0;
}

.small {
  font-weight: 400;
  font-size: 11px;
}

/* 预览页签的顶部工具条：左右切换 + 类型体积 + 复制 */
.preview-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
}

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
  font-size: 11.5px;
  padding: 3px 10px;
}

.seg button.active {
  background: var(--fl-accent-soft);
  color: var(--fl-accent);
  font-weight: 600;
}

.seg button:disabled {
  opacity: 0.4;
}

/* 字符集等提示性小标签，不抢正文注意力 */
.badge {
  display: inline-block;
  margin-left: 6px;
  padding: 1px 6px;
  border-radius: 999px;
  border: 1px solid var(--fl-border);
  background: var(--fl-panel-2);
  color: var(--fl-text-2);
  font-size: 11px;
  font-weight: 400;
  vertical-align: middle;
}

.frames-pane {
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.frames {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 2px 0 8px;
}

.frame {
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  background: var(--fl-panel-2);
  /* 上行/下行用左边框颜色区分方向，比加图标更省空间 */
  border-left: 3px solid var(--fl-border);
}

.frame.up {
  border-left-color: var(--fl-accent);
}

.frame.down {
  border-left-color: var(--fl-green);
}

.frame-head {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 8px;
  border-bottom: 1px solid var(--fl-border);
  font-size: 11px;
}

.frame-head .dir {
  font-weight: 700;
  font-size: 12px;
}

.frame.up .dir {
  color: var(--fl-accent);
}

.frame.down .dir {
  color: var(--fl-green);
}

.kind-tag {
  padding: 0 5px;
  border-radius: 3px;
  background: var(--fl-panel);
  color: var(--fl-muted);
  font-size: 10px;
  text-transform: uppercase;
}

.offset {
  margin-left: auto;
  font-variant-numeric: tabular-nums;
}

.frame-body {
  margin: 0;
  padding: 6px 8px;
  max-height: 200px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
  font-family: var(--fl-mono);
  font-size: 11.5px;
  line-height: 1.5;
}
</style>
