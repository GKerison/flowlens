<script setup lang="ts">
/**
 * 断点处置弹窗：后端在请求/响应阶段挂起后推来的内容在此展示与编辑。
 *
 * 交互约定：
 *  - 队列逐条处理：ui.breakpoints[0] 是当前正在编辑的一条，处置后自动轮到下一条；
 *  - 「放行」提交编辑（method/url/headers/body/status），「取消」把这条请求/响应丢弃；
 *  - 二进制或截断的内容不允许编辑 body（编辑会破坏数据），只放行或丢弃。
 */
import { computed, reactive, ref, watch } from 'vue'
import { App as Backend } from '../../bindings/github.com/kerison/flowlens/internal/service'
import type { BreakpointDecision, BreakpointPayload } from '../types'
import { useUiStore } from '../stores/ui'
import { useI18n } from '../i18n'

const ui = useUiStore()
const { t } = useI18n()

const current = computed<BreakpointPayload | null>(() => ui.breakpoints[0] ?? null)

/* 编辑态：打开新断点时按 payload 初始化 */
const form = reactive({
  method: '',
  url: '',
  headers: [] as Array<{ name: string; value: string }>,
  body: '',
  statusCode: 0,
  resHeaders: [] as Array<{ name: string; value: string }>,
})
const bodyEdited = ref(false)

watch(current, (p) => {
  if (!p) return
  form.method = p.method
  form.url = p.url
  form.headers = p.headers.map((h) => ({ ...h }))
  form.body = p.body
  form.statusCode = p.statusCode ?? 0
  form.resHeaders = (p.resHeaders ?? []).map((h) => ({ ...h }))
  bodyEdited.value = false
})

const isRequest = computed(() => current.value?.stage === 'request')
/** 后端截断或二进制的内容：放行时 body 沿用原值，避免破坏数据 */
const bodyEditable = computed(() => !!current.value && !current.value.bodyBinary && !current.value.bodyTruncated)

function addHeader(): void {
  form.headers.push({ name: '', value: '' })
}

function removeHeader(i: number): void {
  form.headers.splice(i, 1)
}

function addResHeader(): void {
  form.resHeaders.push({ name: '', value: '' })
}

function removeResHeader(i: number): void {
  form.resHeaders.splice(i, 1)
}

/** 逐字符监听 body 输入：只有真的改了才提交新 body，否则沿用原内容。 */
function onBodyInput(): void {
  bodyEdited.value = true
}

async function resolve(action: 'forward' | 'drop'): Promise<void> {
  const p = current.value
  if (!p) return
  if (action === 'forward' && bodyEditable.value && bodyEdited.value && p.body && form.body.length > p.body.length * 2 + 1024) {
    // 防手滑：粘贴了超大内容时拦一下，交给后端上限兜底
    ui.showToast(t('编辑后的内容过大，请检查是否误粘贴'), 'error')
    return
  }

  const decision: BreakpointDecision = { action }
  if (action === 'forward') {
    decision.method = form.method
    decision.url = form.url
    // 响应阶段的头编辑同样走 headers 字段（后端按阶段落到对应对象上）
    decision.headers = isRequest.value ? form.headers : form.resHeaders
    decision.statusCode = form.statusCode
    // body：只有用户真的改过且内容可编辑时才提交新内容；
    // 二进制/截断的内容必须传 null（沿用原始字节），把界面上的预览文本发回去会破坏数据
    decision.body = bodyEditable.value && bodyEdited.value ? form.body : null
  }

  try {
    const ok = (await Backend.ResolveBreakpoint(
      p.holdKey,
      decision as unknown as Parameters<typeof Backend.ResolveBreakpoint>[1],
    )) as unknown as boolean
    if (!ok) ui.showToast(t('该断点已失效（可能已超时放行）'), 'error')
  } catch (err) {
    ui.showToast(err instanceof Error ? err.message : String(err), 'error')
    return // 提交失败时保持弹窗，用户可以重试或取消
  }
  ui.settleBreakpoint(p.holdKey)
}

/** 超时提示：断点默认 120s 自动放行，界面上给用户时间预期。 */
const waitHint = computed(() => {
  const p = current.value
  return p ? (p.stage === 'request' ? t('请求已挂起（超时自动放行）') : t('响应已挂起（超时自动放行）')) : ''
})
</script>

<template>
  <teleport to="body">
    <transition name="bp">
      <div v-if="current" class="bp-overlay">
        <div class="bp-dialog" role="dialog" aria-modal="true" :aria-label="t('断点')">
          <header class="bp-head">
            <span class="bp-badge" :class="isRequest ? 'req' : 'res'">
              {{ isRequest ? t('请求断点') : t('响应断点') }}
            </span>
            <span class="muted small">{{ waitHint }}</span>
            <span class="muted small queue" v-if="ui.breakpoints.length > 1">
              {{ t('还有 {count} 条排队', { count: ui.breakpoints.length - 1 }) }}
            </span>
          </header>

          <div class="bp-body">
            <div class="line">
              <select v-if="isRequest" v-model="form.method" class="method">
                <option>GET</option><option>POST</option><option>PUT</option>
                <option>PATCH</option><option>DELETE</option><option>OPTIONS</option><option>HEAD</option>
              </select>
              <input v-model="form.url" class="mono url" spellcheck="false" aria-label="URL" />
            </div>

            <!-- 请求头 / 响应头。请求与响应分开渲染：v-model 要求可赋值表达式，三元写法不合法 -->
            <section class="headers">
              <h4>{{ isRequest ? t('请求头') : t('响应头') }}</h4>
              <template v-if="isRequest">
                <div v-for="(h, i) in form.headers" :key="'r' + i" class="h-line">
                  <input v-model="h.name" class="mono name" spellcheck="false" :placeholder="t('名称')" />
                  <input v-model="h.value" class="mono val" spellcheck="false" :placeholder="t('值')" />
                  <button class="ghost x" :title="t('删除该头')" @click="removeHeader(i)">✕</button>
                </div>
              </template>
              <template v-else>
                <div v-for="(h, i) in form.resHeaders" :key="'s' + i" class="h-line">
                  <input v-model="h.name" class="mono name" spellcheck="false" :placeholder="t('名称')" />
                  <input v-model="h.value" class="mono val" spellcheck="false" :placeholder="t('值')" />
                  <button class="ghost x" :title="t('删除该头')" @click="removeResHeader(i)">✕</button>
                </div>
              </template>
              <button class="ghost add" @click="isRequest ? addHeader() : addResHeader()">{{ t('+ 添加头') }}</button>
            </section>

            <!-- 状态码（仅响应阶段） -->
            <div v-if="!isRequest" class="line status-line">
              <span class="muted small">{{ t('状态码') }}</span>
              <input v-model.number="form.statusCode" type="number" min="0" max="599" class="tiny" />
              <span class="muted small">{{ t('0 表示沿用原状态码') }}</span>
            </div>

            <!-- body -->
            <section class="body">
              <h4>{{ isRequest ? t('请求体') : t('响应体') }}</h4>
              <textarea
                v-if="bodyEditable"
                v-model="form.body"
                class="mono"
                rows="8"
                spellcheck="false"
                @input="onBodyInput"
              ></textarea>
              <p v-else class="muted small">
                {{ t('内容为二进制或已截断，不提供编辑（编辑会破坏数据）。可直接放行或丢弃。') }}
              </p>
            </section>
          </div>

          <footer class="bp-foot">
            <span class="muted tiny-hint">{{ t('超时后自动放行原内容') }}</span>
            <div class="spacer" />
            <button class="ghost" @click="resolve('drop')">
              {{ isRequest ? t('取消请求') : t('丢弃响应') }}
            </button>
            <button class="primary" @click="resolve('forward')">{{ t('放行') }}</button>
          </footer>
        </div>
      </div>
    </transition>
  </teleport>
</template>

<style scoped>
.bp-overlay {
  position: fixed;
  inset: 0;
  z-index: 60; /* 高于设置抽屉（40）：断点优先级最高 */
  background: color-mix(in srgb, var(--fl-text) 32%, transparent);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 20px;
}

.bp-dialog {
  width: min(680px, 100%);
  max-height: min(86vh, 720px);
  background: var(--fl-bg);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius);
  box-shadow: var(--fl-shadow);
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.bp-head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--fl-border);
  background: var(--fl-panel);
}

.bp-badge {
  font-size: 11px;
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 999px;
}

.bp-badge.req {
  background: var(--fl-accent-soft);
  color: var(--fl-accent);
}

.bp-badge.res {
  background: var(--fl-green-soft, var(--fl-accent-soft));
  color: var(--fl-green, var(--fl-accent));
}

.queue {
  margin-left: auto;
}

.bp-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 14px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.line {
  display: flex;
  gap: 8px;
  align-items: center;
}

.method {
  width: 92px;
  flex: none;
}

.url {
  flex: 1;
  min-width: 0;
}

.headers {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

h4 {
  margin: 0;
  font-size: 12px;
  color: var(--fl-text-2);
}

.h-line {
  display: flex;
  gap: 6px;
}

.h-line .name {
  width: 32%;
  flex: none;
}

.h-line .val {
  flex: 1;
  min-width: 0;
}

.h-line .x {
  flex: none;
  width: 26px;
  padding: 3px 0;
  font-size: 11px;
}

.add {
  align-self: flex-start;
  font-size: 11px;
  padding: 3px 8px;
}

.status-line .tiny {
  width: 80px;
}

.body textarea {
  width: 100%;
  font-size: 12px;
  line-height: 1.5;
  background: var(--fl-panel-2);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  padding: 8px 10px;
  color: var(--fl-text);
  resize: vertical;
  outline: none;
}

.body textarea:focus {
  border-color: var(--fl-accent);
}

.bp-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  border-top: 1px solid var(--fl-border);
  background: var(--fl-panel);
}

.tiny-hint {
  font-size: 11px;
}

.ghost {
  border-color: var(--fl-border);
  background: transparent;
  color: var(--fl-text-2);
}

.bp-enter-active,
.bp-leave-active {
  transition: opacity 0.15s ease;
}

.bp-enter-active .bp-dialog,
.bp-leave-active .bp-dialog {
  transition: transform 0.18s ease;
}

.bp-enter-from,
.bp-leave-to {
  opacity: 0;
}

.bp-enter-from .bp-dialog,
.bp-leave-to .bp-dialog {
  transform: scale(0.98);
}
</style>
