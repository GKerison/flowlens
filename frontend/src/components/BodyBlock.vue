<script setup lang="ts">
/**
 * Body 渲染组件。
 *
 * 安全要点：文本内容一律通过模板插值（文本节点）输出，**绝不**使用 v-html，
 * 否则抓到的 HTML/JS 会在应用内执行，等价于把任意网页脚本引入本地应用。
 */
import { computed } from 'vue'
import type { BodyView } from '../types'
import { useI18n } from '../i18n'

const props = defineProps<{ body?: BodyView }>()
const { t } = useI18n()

const text = computed(() => {
  const b = props.body
  if (!b) return ''
  const raw = b.preview || ''
  if (b.kind !== 'json') return raw
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw // 非法 JSON（如被截断）时保持原样，避免用户什么都看不到
  }
})

const imageSrc = computed(() => {
  const b = props.body
  if (!b || b.kind !== 'image' || !b.base64) return ''
  return `data:${b.mime.split(';')[0]};base64,${b.base64}`
})
</script>

<template>
  <p v-if="!body || body.kind === 'empty'" class="muted">{{ t('无内容') }}</p>
  <img v-else-if="imageSrc" class="preview-img" :src="imageSrc" :alt="t('响应图片')" />
  <p v-else-if="body.kind === 'binary'" class="muted">
    {{ t('二进制内容（{size} 字节），暂不支持预览', { size: body.size }) }}
  </p>
  <pre v-else class="code">{{ text }}</pre>
</template>

<style scoped>
.code {
  margin: 0;
  padding: 10px 12px;
  background: var(--fl-panel-2);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 420px;
  overflow: auto;
  font-family: var(--fl-mono);
  font-size: 12px;
  line-height: 1.6;
}

.preview-img {
  max-width: 100%;
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  background: repeating-conic-gradient(#eee 0% 25%, #fff 0% 50%) 50% / 16px 16px;
}
</style>
