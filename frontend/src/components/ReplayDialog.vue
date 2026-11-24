<script setup lang="ts">
/**
 * 「重放并编辑」弹窗：改方法 / 地址 / 请求头 / 请求体后再发一次。
 *
 * 设计取舍：
 *  - 每次都从源会话重新克隆一份草稿，取消后不残留上次的修改；
 *  - 发送结果就地展示（状态码、耗时、大小、响应体预览），不需要用户再去列表里找新会话；
 *  - 二进制/被截断的请求体不放进文本框：让用户对着乱码改，比直接说"不支持"更糟。
 */
import { computed, ref } from 'vue'
import type { FlowDetail, HeaderPair, ReplayEdit } from '../types'
import { useTrafficStore } from '../stores/traffic'
import { useI18n } from '../i18n'

const traffic = useTrafficStore()
const { t } = useI18n()

const props = defineProps<{ detail: FlowDetail }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const methods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS', 'HEAD']

/** 源请求体是否可以安全地放进文本框编辑。 */
const bodyEditable = computed(() => {
  const b = props.detail.reqBody
  if (!b || b.kind === 'empty') return true
  return b.kind !== 'binary' && b.kind !== 'image'
})

const originalURL = computed(() => {
  const d = props.detail
  const isDefault =
    (d.scheme === 'http' && d.port === '80') || (d.scheme === 'https' && d.port === '443')
  const host = d.port && !isDefault ? `${d.host}:${d.port}` : d.host
  return `${d.scheme}://${host}${d.path}${d.query ? '?' + d.query : ''}`
})

/** 把 headers 展平成有序数组，同名多值各自一行，保持原始顺序。 */
function toRows(headers: Record<string, string[]> | undefined): HeaderPair[] {
  const out: HeaderPair[] = []
  if (!headers) return out
  // 常见头部排在前面，用户最常改的就是这几个
  const priority = ['content-type', 'authorization', 'cookie', 'user-agent', 'accept']
  const keys = Object.keys(headers).sort((a, b) => {
    const ia = priority.indexOf(a.toLowerCase())
    const ib = priority.indexOf(b.toLowerCase())
    if (ia !== ib) return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib)
    return a.localeCompare(b)
  })
  for (const k of keys) {
    for (const v of headers[k]) out.push({ name: k, value: v })
  }
  return out
}

const method = ref(props.detail.method)
const url = ref(originalURL.value)
const rows = ref<HeaderPair[]>(toRows(props.detail.reqHeaders))
const body = ref<string>(props.detail.reqBody?.preview ?? '')

const sending = ref(false)
const error = ref('')
const result = ref<FlowDetail | null>(null)

function reset(): void {
  method.value = props.detail.method
  url.value = originalURL.value
  rows.value = toRows(props.detail.reqHeaders)
  body.value = props.detail.reqBody?.preview ?? ''
  error.value = ''
  result.value = null
}

function addRow(): void {
  rows.value.push({ name: '', value: '' })
}

function removeRow(i: number): void {
  rows.value.splice(i, 1)
}

function buildEdit(): ReplayEdit {
  return {
    method: method.value,
    url: url.value.trim(),
    // 空名行直接丢弃：用户点了"添加一行"又没填，不该因此报错
    headers: rows.value.filter((r) => r.name.trim() !== ''),
    // 二进制请求体进不了文本框，此时传 null 表示"沿用原请求体"
    body: bodyEditable.value ? body.value : null,
  }
}

async function send(): Promise<void> {
  error.value = ''
  result.value = null
  const edit = buildEdit()
  if (!edit.url) {
    error.value = t('请填写请求地址')
    return
  }
  sending.value = true
  try {
    result.value = await traffic.replayFlowEdit(props.detail.id, edit)
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err)
  } finally {
    sending.value = false
  }
}

function formatSize(bytes: number): string {
  if (!bytes) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

/** 响应预览只取前 2000 字：弹窗是"看一眼有没有通"，不是详情页。 */
const resultPreview = computed(() => {
  const b = result.value?.resBody
  if (!b || !b.preview) return ''
  return b.preview.length > 2000 ? b.preview.slice(0, 2000) + '\n' + t('…（已省略）') : b.preview
})
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog">
      <header class="head">
        <strong>{{ t('重放并编辑') }}</strong>
        <span class="muted small">{{ t('改动只影响这一次发送，不会修改原会话') }}</span>
        <button class="close" :title="t('关闭')" @click="emit('close')">✕</button>
      </header>

      <div class="body">
        <div class="row">
          <select v-model="method" class="method">
            <option v-for="m in methods" :key="m" :value="m">{{ m }}</option>
          </select>
          <input v-model="url" class="url mono" spellcheck="false" placeholder="https://…" />
        </div>

        <section>
          <h4>
            {{ t('请求头') }}
            <button class="ghost tiny" @click="addRow">{{ t('+ 添加') }}</button>
          </h4>
          <div class="headers">
            <div v-for="(h, i) in rows" :key="i" class="header-row">
              <input v-model="h.name" class="mono" :placeholder="t('名称')" spellcheck="false" />
              <input v-model="h.value" class="mono" :placeholder="t('值')" spellcheck="false" />
              <button class="ghost tiny" :title="t('删除该行')" @click="removeRow(i)">✕</button>
            </div>
            <p v-if="rows.length === 0" class="muted small">{{ t('没有请求头，将只发送浏览器默认头') }}</p>
          </div>
        </section>

        <section>
          <h4>
            {{ t('请求体') }}
            <span v-if="!bodyEditable" class="badge">{{ t('二进制内容，无法在此编辑（将沿用原请求体）') }}</span>
            <span v-else-if="detail.reqBody?.truncated" class="badge">
              {{ t('原请求体已截断，编辑后将发送不完整内容') }}
            </span>
          </h4>
          <textarea
            v-model="body"
            class="mono body-input"
            rows="7"
            spellcheck="false"
            :disabled="!bodyEditable"
            :placeholder="t('（无请求体）')"
          />
        </section>

        <p v-if="error" class="alert">{{ error }}</p>

        <div v-if="result" class="result">
          <div class="result-head">
            <span
              class="status"
              :class="{ bad: !result.statusCode || result.statusCode >= 400 }"
            >
              {{ result.statusCode || t('无响应') }}
            </span>
            <span class="muted small">{{ result.durationMs }} ms</span>
            <span class="muted small">{{ formatSize(result.resSize) }}</span>
            <span class="muted small">{{ t('新会话已加入列表') }}</span>
          </div>
          <p v-if="result.error" class="alert">{{ t('请求失败：{error}', { error: result.error }) }}</p>
          <pre v-if="resultPreview" class="preview">{{ resultPreview }}</pre>
          <p v-else-if="!result.error" class="muted small">{{ t('（响应体为空或未内联）') }}</p>
        </div>
      </div>

      <footer class="foot">
        <button class="ghost" :disabled="sending" @click="reset">{{ t('重置') }}</button>
        <div class="spacer" />
        <button :disabled="sending" @click="emit('close')">{{ t('关闭') }}</button>
        <button class="primary" :disabled="sending" @click="send">
          {{ sending ? t('发送中…') : t('发送') }}
        </button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.mask {
  position: fixed;
  inset: 0;
  z-index: 40;
  background: rgb(0 0 0 / 0.38);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}

.dialog {
  width: min(760px, 100%);
  max-height: 100%;
  display: flex;
  flex-direction: column;
  background: var(--fl-panel);
  border: 1px solid var(--fl-border-strong);
  border-radius: var(--fl-radius);
  box-shadow: 0 18px 48px rgb(0 0 0 / 0.28);
  overflow: hidden;
}

.head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--fl-border);
  flex: none;
}

.head .close {
  margin-left: auto;
  border: none;
  background: transparent;
  color: var(--fl-muted);
  padding: 2px 6px;
}

.body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.row {
  display: flex;
  gap: 8px;
}

.method {
  flex: none;
  width: 104px;
}

.url {
  flex: 1;
  min-width: 0;
}

h4 {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0 0 6px;
  font-size: 12px;
  font-weight: 600;
  color: var(--fl-text-2);
}

.headers {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.header-row {
  display: flex;
  gap: 6px;
  align-items: center;
}

.header-row input:first-child {
  width: 34%;
  flex: none;
}

.header-row input:nth-child(2) {
  flex: 1;
  min-width: 0;
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
}

.body-input {
  width: 100%;
  resize: vertical;
  min-height: 96px;
  line-height: 1.55;
}

.badge {
  padding: 1px 6px;
  border-radius: 999px;
  border: 1px solid var(--fl-border);
  background: var(--fl-panel-2);
  color: var(--fl-muted);
  font-size: 11px;
  font-weight: 400;
}

.alert {
  margin: 0;
  background: var(--fl-red-soft);
  color: var(--fl-red);
  border-radius: var(--fl-radius-sm);
  padding: 8px 10px;
  font-size: 12px;
  word-break: break-all;
}

.result {
  border-top: 1px dashed var(--fl-border);
  padding-top: 10px;
}

.result-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 6px;
}

.status {
  font-weight: 700;
  font-size: 12px;
  color: var(--fl-green);
}

.status.bad {
  color: var(--fl-red);
}

.preview {
  margin: 0;
  padding: 8px 10px;
  max-height: 240px;
  overflow: auto;
  background: var(--fl-panel-2);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  white-space: pre-wrap;
  word-break: break-word;
  font-family: var(--fl-mono);
  font-size: 11.5px;
  line-height: 1.55;
}

.foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  border-top: 1px solid var(--fl-border);
  flex: none;
}

.spacer {
  flex: 1;
}

.small {
  font-size: 11px;
}
</style>
