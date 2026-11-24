<script setup lang="ts">
/**
 * 「预览」页签的内容渲染：把 body 按类型整理成"一眼能看懂"的形态。
 *
 * 安全底线与 BodyBlock 相同：所有文本都走模板插值，**绝不** v-html。
 * HTML 响应只展示源码，不渲染——渲染抓到的网页等价于在本机执行对方的脚本。
 */
import { computed, ref } from 'vue'
import type { BodyView } from '../types'
import { formatSize } from '../format'
import { useI18n } from '../i18n'

const props = defineProps<{ body?: BodyView }>()
const { t } = useI18n()

/** 超过这个字符数就不做语法高亮：几万个 span 会让 WebView 明显卡顿。 */
const HIGHLIGHT_LIMIT = 200_000

const zoomed = ref(false)

const imgSrc = computed(() => {
  const b = props.body
  if (!b || b.kind !== 'image' || !b.base64) return ''
  return `data:${b.mime.split(';')[0]};base64,${b.base64}`
})

/** application/x-www-form-urlencoded 拆成表格，比一行 `a=1&b=2` 好读得多。 */
const formPairs = computed<Array<[string, string]>>(() => {
  const b = props.body
  if (!b?.preview || !/x-www-form-urlencoded/i.test(b.mime)) return []
  return [...new URLSearchParams(b.preview).entries()]
})

/** 能解析时给出格式化后的 JSON，解析失败返回 null（被截断的 JSON 很常见）。 */
const pretty = computed(() => {
  const b = props.body
  if (!b || b.kind !== 'json' || !b.preview) return null
  try {
    return JSON.stringify(JSON.parse(b.preview), null, 2)
  } catch {
    return null
  }
})

interface Token {
  text: string
  /** k=键 s=字符串 d=数字 b=布尔 n=null p=标点 */
  cls: string
}

/** 匹配字符串、数字与字面量；其余（标点、缩进、换行）保持原样。 */
const TOKEN = /"(?:\\.|[^"\\])*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|\b(?:true|false|null)\b/g

const jsonTokens = computed<Token[]>(() => {
  const text = pretty.value
  if (!text || text.length > HIGHLIGHT_LIMIT) return []
  const out: Token[] = []
  let last = 0
  for (const m of text.matchAll(TOKEN)) {
    const at = m.index ?? 0
    if (at > last) out.push({ text: text.slice(last, at), cls: 'p' })
    const raw = m[0]
    if (raw.startsWith('"')) {
      // 字符串后面紧跟冒号才是键，否则是值
      out.push({ text: raw, cls: /^\s*:/.test(text.slice(at + raw.length, at + raw.length + 8)) ? 'k' : 's' })
    } else if (raw === 'true' || raw === 'false') {
      out.push({ text: raw, cls: 'b' })
    } else if (raw === 'null') {
      out.push({ text: raw, cls: 'n' })
    } else {
      out.push({ text: raw, cls: 'd' })
    }
    last = at + raw.length
  }
  if (last < text.length) out.push({ text: text.slice(last), cls: 'p' })
  return out
})

const hasContent = computed(() => !!props.body && props.body.kind !== 'empty')
</script>

<template>
  <div class="preview">
    <p v-if="!hasContent" class="muted">{{ t('无内容') }}</p>

    <!-- 图片：直接看，点击在「适应宽度 / 原始尺寸」之间切换 -->
    <template v-else-if="body?.kind === 'image'">
      <img
        v-if="imgSrc"
        class="pic"
        :class="{ zoomed }"
        :src="imgSrc"
        :title="zoomed ? t('点击缩小到适应宽度') : t('点击按原始尺寸查看')"
        :alt="t('响应图片')"
        @click="zoomed = !zoomed"
      />
      <p v-else class="muted">
        {{ t('图片体积超出内联上限（{size}），未随详情传输。', { size: formatSize(body.size) }) }}
      </p>
      <p class="caption muted">
        {{ body.mime || t('未知类型') }} · {{ formatSize(body.size) }}
        <span v-if="imgSrc"> {{ t('· 点击图片切换缩放') }}</span>
      </p>
    </template>

    <!-- 表单：键值成对，比一行 &amp; 拼接的原文更适合核对参数 -->
    <table v-else-if="formPairs.length" class="form">
      <tbody>
        <tr v-for="[k, v] in formPairs" :key="k">
          <td class="fk">{{ k }}</td>
          <td class="fv mono">{{ v }}</td>
        </tr>
      </tbody>
    </table>

    <!-- JSON：格式化 + 语法高亮 -->
    <pre v-else-if="jsonTokens.length" class="code json"><span v-for="(t, i) in jsonTokens" :key="i" :class="t.cls">{{ t.text }}</span></pre>

    <template v-else>
      <p v-if="body?.kind === 'json'" class="note">
        {{ t('内容不是完整 JSON（可能被截断或不是 JSON），已按原文显示。') }}
      </p>
      <p v-else-if="body?.kind === 'html' || body?.kind === 'xml'" class="note">
        {{ t('按源码显示，不做渲染：渲染抓到的页面等于在本机执行对方的脚本。') }}
      </p>
      <pre v-if="body?.preview" class="code">{{ body.preview }}</pre>
      <p v-else class="muted">{{ t('二进制内容（{size}），暂不支持预览。', { size: formatSize(body?.size ?? 0) }) }}</p>
    </template>

    <p v-if="body?.truncated" class="note">
      {{ t('内容较大，仅预览开头部分（原始 {size}）。', { size: formatSize(body.size) }) }}
    </p>
    <p v-if="body?.encoding && body.encoding !== 'utf-8'" class="note">
      {{ t('原字符集 {encoding}，已转 UTF-8 显示。', { encoding: body.encoding }) }}
    </p>
  </div>
</template>

<style scoped>
.preview {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.pic {
  max-width: 100%;
  align-self: flex-start;
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  cursor: zoom-in;
  /* 棋盘格背景：透明 PNG 才看得出边界，纯白背景下会以为图片被裁了 */
  background: repeating-conic-gradient(#e9ecf1 0% 25%, #fff 0% 50%) 50% / 16px 16px;
}

.pic.zoomed {
  max-width: none;
  cursor: zoom-out;
}

.caption {
  font-size: 11px;
}

.code {
  margin: 0;
  padding: 10px 12px;
  background: var(--fl-panel-2);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 460px;
  overflow: auto;
  font-family: var(--fl-mono);
  font-size: 12px;
  line-height: 1.6;
}

/* 语法高亮：只上色，不加粗，避免大段 JSON 花成一片 */
.json .k {
  color: var(--fl-accent);
}

.json .s {
  color: var(--fl-green);
}

.json .d {
  color: var(--fl-purple);
}

.json .b,
.json .n {
  color: var(--fl-orange);
}

.form {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
}

.form td {
  padding: 4px 8px;
  border-bottom: 1px solid var(--fl-border);
  vertical-align: top;
}

.form .fk {
  width: 36%;
  color: var(--fl-text-2);
  word-break: break-word;
}

.form .fv {
  word-break: break-all;
}

.note {
  margin: 0;
  font-size: 11.5px;
  color: var(--fl-orange);
}
</style>
