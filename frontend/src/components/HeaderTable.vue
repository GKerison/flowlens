<script setup lang="ts">
/**
 * 请求头 / 响应头表格。
 *
 * 复制需求来自真实排查场景：抓包时经常要把某个头的值贴进 Postman、curl 或搜索框，
 * 手工拖选很容易漏字符（尤其是长 token）。这里给出三种粒度：
 * 头名称、值、以及 "名称: 值" 整行（多值头按多行输出，可直接粘到请求构造器里）。
 */
import { computed } from 'vue'
import { useUiStore } from '../stores/ui'
import { useI18n } from '../i18n'

const ui = useUiStore()
const { t } = useI18n()
import { headerRows, type HeaderRow } from '../format'

const props = defineProps<{
  headers?: Record<string, string[]>
  /** 没有头时的提示文案 */
  empty?: string
}>()

const rows = computed<HeaderRow[]>(() => headerRows(props.headers))

function copy(row: HeaderRow, what: 'name' | 'value' | 'line'): void {
  if (what === 'name') {
    ui.copyText(row.name, t('已复制头名称 {name}', { name: row.name }))
    return
  }
  if (what === 'value') {
    ui.copyText(row.values.join(', '), t('已复制 {name} 的值', { name: row.name }))
    return
  }
  // 多值头拆成多行：合成一行再粘回去会被服务端当成一个值
  ui.copyText(row.values.map((v) => `${row.name}: ${v}`).join('\n'), t('已复制 名称: 值'))
}
</script>

<template>
  <table class="headers">
    <tbody>
      <tr v-for="row in rows" :key="row.name">
        <td class="key">{{ row.name }}</td>
        <td class="val mono">{{ row.joined }}</td>
        <td class="ops">
          <button class="act" :title="t('复制头名称')" @click="copy(row, 'name')">K</button>
          <button class="act" :title="t('复制值')" @click="copy(row, 'value')">V</button>
          <button class="act wide" :title="t('复制「名称: 值」')" @click="copy(row, 'line')">K:V</button>
        </td>
      </tr>
      <tr v-if="rows.length === 0">
        <td class="muted" colspan="3">{{ empty || t('无') }}</td>
      </tr>
    </tbody>
  </table>
</template>

<style scoped>
.headers {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
  table-layout: fixed;
}

.headers td {
  padding: 3px 8px;
  border-bottom: 1px solid var(--fl-border);
  vertical-align: top;
}

.headers .key {
  width: 34%;
  color: var(--fl-text-2);
  word-break: break-word;
}

.headers .val {
  word-break: break-all;
}

.headers .ops {
  width: 82px;
  padding-right: 0;
  white-space: nowrap;
  text-align: right;
}

/* 操作按钮常驻但压暗：表格信息密度高，不希望它抢注意力，
   可完全隐藏又会让用户根本不知道能复制。悬停行/聚焦时提亮到正常对比度。 */
.headers .ops .act {
  opacity: 0.45;
  padding: 0 4px;
  margin-left: 2px;
  font-size: 10px;
  line-height: 16px;
  border-radius: 4px;
  color: var(--fl-muted);
  background: transparent;
  border-color: var(--fl-border);
}

.headers .ops .act.wide {
  padding: 0 5px;
}

.headers tr:hover .act,
.headers .act:focus-visible {
  opacity: 1;
}

.headers .act:hover {
  color: var(--fl-accent);
  border-color: var(--fl-accent);
  background: var(--fl-accent-soft);
}
</style>
