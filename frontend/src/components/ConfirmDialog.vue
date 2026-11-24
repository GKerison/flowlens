<script setup lang="ts">
/**
 * 应用内确认框。
 *
 * 存在的原因：macOS 的 WKWebView 不实现 window.confirm，
 * 调用直接返回 false 且没有任何报错——用户会以为"按钮坏了"。
 * 这里用普通 DOM 实现，行为在三平台完全一致，也能被样式主题化。
 */
import { nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useUiStore } from '../stores/ui'
import { useI18n } from '../i18n'

const ui = useUiStore()
const { t } = useI18n()

const okRef = ref<HTMLButtonElement | null>(null)
const cancelRef = ref<HTMLButtonElement | null>(null)

// 破坏性操作把焦点放在"取消"上：用户习惯性敲回车时不会误删
watch(
  () => ui.confirm.open,
  (open) => {
    if (!open) return
    void nextTick(() => {
      const btn = ui.confirm.danger ? cancelRef.value : okRef.value
      btn?.focus()
    })
  },
)

/**
 * 键盘处理挂在捕获阶段并阻止冒泡：
 * 列表层同样监听 window 的 Esc/Delete，确认框打开时这些快捷键必须先让位。
 */
function onKeydown(ev: KeyboardEvent): void {
  if (!ui.confirm.open) return
  if (ev.key === 'Escape') {
    ev.preventDefault()
    ev.stopPropagation()
    ui.settleConfirm(false)
    return
  }
  if (ev.key === 'Enter') {
    ev.preventDefault()
    ev.stopPropagation()
    ui.settleConfirm(true)
  }
}

onMounted(() => window.addEventListener('keydown', onKeydown, true))
onUnmounted(() => window.removeEventListener('keydown', onKeydown, true))
</script>

<template>
  <transition name="confirm">
    <div v-if="ui.confirm.open" class="overlay" @click.self="ui.settleConfirm(false)">
      <div
        class="dialog"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="fl-confirm-title"
        aria-describedby="fl-confirm-msg"
      >
        <h3 id="fl-confirm-title">{{ ui.confirm.title }}</h3>
        <p id="fl-confirm-msg" class="msg">{{ ui.confirm.message }}</p>
        <div class="actions">
          <button ref="cancelRef" class="ghost" @click="ui.settleConfirm(false)">{{ t('取消') }}</button>
          <button
            ref="okRef"
            :class="ui.confirm.danger ? 'danger solid' : 'primary'"
            @click="ui.settleConfirm(true)"
          >
            {{ ui.confirm.confirmText }}
          </button>
        </div>
      </div>
    </div>
  </transition>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 40;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgb(0 0 0 / 0.32);
}

.dialog {
  width: min(360px, calc(100vw - 48px));
  background: var(--fl-panel);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius);
  box-shadow: var(--fl-shadow);
  padding: 16px 18px 14px;
}

h3 {
  margin: 0 0 8px;
  font-size: 13px;
  font-weight: 650;
}

.msg {
  margin: 0 0 16px;
  color: var(--fl-text-2);
  font-size: 12.5px;
  line-height: 1.6;
  word-break: break-word;
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}

.ghost {
  border-color: var(--fl-border-strong);
  background: transparent;
  color: var(--fl-text-2);
}

.danger.solid {
  background: var(--fl-red);
  border-color: var(--fl-red);
  color: #fff;
}

.danger.solid:hover {
  filter: brightness(1.06);
  background: var(--fl-red);
}

.confirm-enter-active,
.confirm-leave-active {
  transition: opacity 0.18s ease;
}

.confirm-enter-active .dialog,
.confirm-leave-active .dialog {
  transition: transform 0.24s var(--fl-ease);
}

.confirm-enter-from,
.confirm-leave-to {
  opacity: 0;
}

.confirm-enter-from .dialog,
.confirm-leave-to .dialog {
  transform: translateY(12px) scale(0.96);
}
</style>
