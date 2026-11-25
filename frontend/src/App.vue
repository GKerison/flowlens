<script setup lang="ts">
/**
 * 应用外壳：顶栏（状态与主操作）+ 流量主页 + 右上角设置抽屉。
 *
 * 导航只保留一个主页面（流量）：抓包是使用频率最高的动作，其余
 * （配置、证书接入）都是低频操作，收进设置抽屉避免常驻侧栏占用宽度。
 */
import { computed, onMounted, onUnmounted, ref } from 'vue'
import TrafficView from './components/TrafficView.vue'
import SettingsView from './components/SettingsView.vue'
import ConfirmDialog from './components/ConfirmDialog.vue'
import BreakpointDialog from './components/BreakpointDialog.vue'
import { init } from './stores/app'
import { useConfigStore } from './stores/config'
import { useUiStore } from './stores/ui'
import { useI18n } from './i18n'

onMounted(init)

// 启动后静默检查一次更新：结果只体现在设置页的角标上，不打断抓包。
// 延迟 3 秒避开启动阶段的文件与网络 IO；失败也不提示，用户可随时手动检查。
let updateTimer = 0
onMounted(() => {
  updateTimer = window.setTimeout(() => void config.checkUpdate(true), 3000)
})

const ui = useUiStore()
const config = useConfigStore()
const { t } = useI18n()

/**
 * 状态即操作：抓包中/已暂停是同一个可点击控件的两种状态，
 * 不再单独放一个「暂停」按钮，避免顶栏出现两个表达同一件事的入口。
 */
const status = computed(() => {
  if (!config.proxy.running) {
    return { text: t('已停止'), cls: 'stopped', title: t('代理未运行'), disabled: true }
  }
  if (config.proxy.paused) {
    return { text: t('已暂停抓取'), cls: 'paused', title: t('点击恢复解密抓包（当前仅中转，上网不受影响）'), disabled: false }
  }
  return {
    text: t('抓包中'),
    cls: 'running',
    title: t('点击暂停抓取（暂停后仅中转，上网不受影响）'),
    disabled: false,
  }
})

interface AddressOption {
  label: string
  address: string
  hint: string
}

/**
 * 顶栏地址下拉：0.0.0.0 是监听语义，不是可填的地址。
 * 本机给 127.0.0.1，手机给真实局域网 IP（虚拟网卡地址后端已过滤）。
 */
const addressOptions = computed<AddressOption[]>(() => {
  const local = config.bootstrap?.proxyUrl || ''
  const options: AddressOption[] = [
    { label: t('本机'), address: local, hint: t('本机可直接连接') },
  ]
  const port = config.config?.proxyPort
  if (config.config?.listenHost === '0.0.0.0' && port) {
    for (const ip of config.bootstrap?.lanIps || []) {
      options.push({
        label: t('手机 / 局域网'),
        address: `http://${ip}:${port}`,
        hint: ip,
      })
    }
  }
  return options
})

/** 顶栏只显示地址与端口，凭据只在复制内容与悬停说明里出现，避免密码留在屏幕上。 */
const authOn = computed(() => !!config.config?.requireAuth)

const addrOpen = ref(false)
const addrRef = ref<HTMLElement | null>(null)

function toggleAddresses(): void {
  addrOpen.value = !addrOpen.value
}

function copyAddress(option: AddressOption): void {
  addrOpen.value = false
  void ui.copyText(
    config.withCredentials(option.address),
    authOn.value ? t('代理地址已复制（已含用户名密码）') : t('代理地址已复制'),
  )
}

function onPointerDown(ev: PointerEvent): void {
  if (addrOpen.value && addrRef.value && !addrRef.value.contains(ev.target as Node)) {
    addrOpen.value = false
  }
}
function onEscape(ev: KeyboardEvent): void {
  if (ev.key === 'Escape' && addrOpen.value) addrOpen.value = false
}
onMounted(() => {
  window.addEventListener('pointerdown', onPointerDown)
  window.addEventListener('keydown', onEscape, true)
})
onUnmounted(() => {
  window.clearTimeout(updateTimer)
  window.removeEventListener('pointerdown', onPointerDown)
  window.removeEventListener('keydown', onEscape, true)
})

const addrTitle = computed(() =>
  authOn.value
    ? t('点击选择要复制的代理地址（含认证凭据）')
    : t('点击选择要复制的代理地址'),
)

/** 系统代理开关的悬停说明：把「会发生什么」说清楚，避免误点。 */
const sysProxyTitle = computed(() => {
  const s = config.sysProxy
  if (!s) return t('系统代理状态未知')
  if (!s.supported) return s.detail
  if (s.enabled) return t('系统代理当前指向 {addr}，点击恢复原设置', { addr: s.addr })
  const base = t('把系统代理设为 {target}，所有本机流量经过 观流', { target: s.target })
  return s.needsAdmin ? `${base}${t('（需要管理员授权）')}` : base
})

/** 顶部横幅：启动自愈的结论，或上次未恢复的残留。 */
const sysBanner = computed(() => {
  const s = config.sysProxy
  if (!s) return null
  if (s.stale) {
    return {
      kind: 'warn',
      text:
        config.sysProxyNote ||
        t('上次未正常退出：系统代理可能仍指向 观流，恢复前请不要关闭本应用。'),
    }
  }
  if (config.sysProxyNote) return { kind: 'info', text: config.sysProxyNote }
  return null
})
</script>

<template>
  <div class="app">
    <header class="topbar">
      <div class="brand">
        <span
          class="gauge"
          :class="{
            live: config.proxy.running && !config.proxy.paused,
            paused: config.proxy.running && config.proxy.paused,
          }"
          :title="status.title"
          aria-hidden="true"
        >
          <svg viewBox="0 0 24 24">
            <circle class="gauge-track" cx="12" cy="12" r="9" />
            <circle class="gauge-arc" cx="12" cy="12" r="9" />
            <line class="gauge-needle" x1="12" y1="12" x2="12" y2="5.5" />
            <circle class="gauge-hub" cx="12" cy="12" r="1.3" />
          </svg>
        </span>
        <span
          class="name"
          :class="{ shimmer: config.proxy.running && !config.proxy.paused }"
        >观流</span>
      </div>

      <button
        class="pill"
        :class="status.cls"
        :disabled="status.disabled"
        :title="status.title"
        @click="config.togglePause()"
      >
        <i class="dot" />
        {{ status.text }}
      </button>

      <div v-if="ui.style !== 'simple'" ref="addrRef" class="addr-wrap">
        <button
          class="addr"
          :class="{ open: addrOpen }"
          :title="addrTitle"
          :aria-expanded="addrOpen"
          @click="toggleAddresses"
        >
          <span class="mono">{{ addressOptions[0]?.address || '—' }}</span>
          <span v-if="authOn" class="lock" :title="t('已开启代理认证')">🔒</span>
          <span class="caret">▾</span>
        </button>
        <transition name="menu">
          <div v-if="addrOpen" class="addr-menu" role="menu">
            <div class="menu-head">{{ t('点击复制代理地址') }}</div>
            <button
              v-for="option in addressOptions"
              :key="option.address"
              role="menuitem"
              class="menu-item"
              @click="copyAddress(option)"
            >
              <span class="menu-label">{{ option.label }}</span>
              <span class="menu-addr mono">{{ option.address }}</span>
              <span class="menu-hint muted">{{ option.hint }}</span>
            </button>
            <p class="menu-foot muted">
              {{ t('0.0.0.0 是监听地址，填在客户端请用上面这些实际地址') }}
            </p>
          </div>
        </transition>
      </div>

      <div class="spacer" />

      <button
        v-if="ui.style !== 'simple' && config.sysProxy?.supported"
        class="ghost sysproxy"
        :class="{ on: config.sysProxy.enabled }"
        :disabled="config.sysProxyBusy"
        :title="sysProxyTitle"
        @click="config.toggleSysProxy()"
      >
        <i class="sdot" />
        {{ t('系统代理') }}
      </button>

      <button class="primary" @click="config.toggleProxy()">
        {{ config.proxy.running ? t('停止代理') : t('启动代理') }}
      </button>

      <button
        class="ghost icon gear"
        :title="t('设置（网络、映射、主题、证书与接入）')"
        @click="ui.openSettings()"
      >
        ⚙
      </button>
    </header>

    <div v-if="config.proxy.lastError" class="banner">
      {{ config.proxy.lastError }}
    </div>

    <div v-if="sysBanner" class="banner" :class="sysBanner.kind">
      <span class="banner-text">{{ sysBanner.text }}</span>
      <button v-if="config.sysProxy?.stale" class="small" @click="config.disableSysProxy()">
        {{ t('一键恢复') }}
      </button>
      <button v-else class="small" @click="config.sysProxyNote = ''">{{ t('知道了') }}</button>
    </div>

    <main class="content">
      <TrafficView />
    </main>

    <!-- 设置抽屉：低频操作统一入口，证书接入也在里面 -->
    <SettingsView />

    <transition name="toast">
      <div v-if="ui.toast" class="toast" :class="ui.toast.kind">
        {{ ui.toast.text }}
      </div>
    </transition>

    <!-- 全局只挂一个确认框：任何页面/弹窗都能用 ui.confirmAction 触发 -->
    <ConfirmDialog />
    <!-- 断点处置弹窗：代理挂起请求/响应时弹出，处置优先级最高 -->
    <BreakpointDialog />
  </div>
</template>

<style scoped>
.app {
  display: flex;
  flex-direction: column;
  height: 100%;
  overflow: hidden;
}

.topbar {
  /* 入场动画的 transform 会产生层叠上下文：不抬高顶栏层级，地址菜单会被后面的内容区盖住 */
  position: relative;
  z-index: 20;
  height: var(--fl-topbar-h);
  flex: none;
  display: flex;
  align-items: center;
  gap: var(--fl-topbar-gap);
  padding: 0 var(--fl-topbar-pad-x);
  background: var(--fl-panel);
  border-bottom: 1px solid var(--fl-border);
  user-select: none;
  animation: fl-drop 0.5s var(--fl-ease) both;
}

.brand {
  display: flex;
  align-items: center;
  gap: 7px;
}

/* 小仪表盘：停止时是静态刻度，抓包时弧线旋转 + 指针摆动，暂停时停在橙色刻度 */
.gauge {
  width: 19px;
  height: 19px;
  flex: none;
  display: inline-flex;
  color: var(--fl-muted);
}

.gauge svg {
  width: 100%;
  height: 100%;
  overflow: visible;
}

.gauge-track,
.gauge-arc {
  fill: none;
  stroke-width: 2.6;
}

.gauge-track {
  stroke: var(--fl-border-strong);
  stroke-dasharray: 2.5 3;
}

.gauge-arc {
  stroke: currentColor;
  stroke-linecap: round;
  stroke-dasharray: 15 42;
  transform-origin: 12px 12px;
  transform: rotate(-55deg);
  opacity: 0.9;
}

.gauge-needle {
  stroke: currentColor;
  stroke-width: 1.7;
  stroke-linecap: round;
  transform-origin: 12px 12px;
  transform: rotate(-55deg);
}

.gauge-hub {
  fill: currentColor;
}

.gauge.live {
  color: var(--fl-accent);
  filter: drop-shadow(0 0 3px color-mix(in srgb, var(--fl-accent) 55%, transparent));
}

.gauge.live .gauge-arc {
  animation: fl-gauge-ring 1.8s linear infinite;
}

.gauge.live .gauge-needle {
  animation: fl-gauge-needle 1.1s ease-in-out infinite alternate;
}

.gauge.paused {
  color: var(--fl-orange);
}

@keyframes fl-gauge-ring {
  to {
    transform: rotate(305deg);
  }
}

@keyframes fl-gauge-needle {
  from {
    transform: rotate(-62deg);
  }
  to {
    transform: rotate(62deg);
  }
}

.name {
  font-size: 15px;
  font-weight: 650;
  letter-spacing: 0.5px;
}

/* 抓包中让应用名扫过一道高对比流光；停止/暂停后立即回到静态文字 */
.brand .name.shimmer {
  background: linear-gradient(
    100deg,
    var(--fl-text) 0%,
    var(--fl-text) 34%,
    var(--fl-accent) 43%,
    #fff 50%,
    var(--fl-accent) 57%,
    var(--fl-text) 66%,
    var(--fl-text) 100%
  );
  background-size: 260% 100%;
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
  filter: drop-shadow(0 0 3px color-mix(in srgb, var(--fl-accent) 45%, transparent));
  animation: fl-shine 2.2s linear infinite;
}

@keyframes fl-shine {
  from {
    background-position: 190% 0;
  }
  to {
    background-position: -90% 0;
  }
}

@media (prefers-reduced-motion: reduce) {
  .brand .name.shimmer {
    background: none;
    -webkit-text-fill-color: currentColor;
    filter: none;
    animation: none;
  }

  .gauge.live .gauge-arc,
  .gauge.live .gauge-needle {
    animation: none;
  }
}

.ver {
  font-size: 10px;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  padding: 3px 9px;
  border-radius: 999px;
  border: 1px solid var(--fl-border);
  color: var(--fl-text-2);
  background: var(--fl-panel-2);
}

.pill .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--fl-muted);
}

.pill.running {
  color: var(--fl-green);
  background: var(--fl-green-soft);
  border-color: transparent;
}

.pill.running .dot {
  background: var(--fl-green);
  animation: fl-breathe 2.6s ease-in-out infinite;
}

.pill.paused {
  color: var(--fl-orange);
  background: var(--fl-orange-soft);
  border-color: transparent;
}

.pill.paused .dot {
  background: var(--fl-orange);
}

.pill:not(:disabled):hover {
  border-color: var(--fl-border-strong);
  filter: brightness(1.05);
}

.pill:disabled {
  cursor: default;
  opacity: 0.75;
}

.addr-wrap {
  position: relative;
  flex: none;
}

.addr {
  border: 1px solid var(--fl-border);
  background: var(--fl-panel-2);
  color: var(--fl-text-2);
  font-size: 11px;
  padding: 3px 9px;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  transition:
    border-color 0.18s ease,
    color 0.18s ease,
    background 0.18s ease,
    box-shadow 0.18s ease;
}

.addr .lock {
  font-size: 10px;
  opacity: 0.75;
}

.addr .caret {
  font-size: 9px;
  opacity: 0.55;
  transition: transform 0.2s cubic-bezier(0.2, 0.8, 0.2, 1);
}

.addr.open .caret {
  transform: rotate(180deg);
}

.addr:hover,
.addr.open {
  color: var(--fl-text);
  border-color: var(--fl-border-strong);
  background: var(--fl-panel);
  box-shadow: 0 2px 10px rgb(0 0 0 / 0.06);
}

/* 地址下拉：本机地址排第一，局域网地址只在 LAN 模式出现 */
.addr-menu {
  position: absolute;
  top: calc(100% + 8px);
  left: 0;
  z-index: 35;
  min-width: 270px;
  padding: 6px;
  border: 1px solid var(--fl-border);
  border-radius: 10px;
  background: var(--fl-panel);
  box-shadow: var(--fl-shadow);
  transform-origin: top left;
}

.menu-head {
  padding: 6px 8px 4px;
  font-size: 11px;
  color: var(--fl-muted);
}

.menu-item {
  display: grid;
  grid-template-columns: 68px 1fr;
  align-items: center;
  gap: 2px 8px;
  width: 100%;
  padding: 7px 8px;
  border: none;
  border-radius: 7px;
  background: transparent;
  color: var(--fl-text);
  text-align: left;
  transition: background 0.15s ease;
}

.menu-item:hover,
.menu-item:focus-visible {
  background: var(--fl-accent-soft);
}

.menu-label {
  font-size: 11px;
  font-weight: 600;
  color: var(--fl-accent);
}

.menu-addr {
  font-size: 12px;
  overflow-wrap: anywhere;
}

.menu-hint {
  grid-column: 2;
  font-size: 10px;
}

.menu-foot {
  margin: 4px 8px 2px;
  font-size: 10px;
  line-height: 1.5;
}

.menu-enter-active,
.menu-leave-active {
  transition:
    opacity 0.16s ease,
    transform 0.18s cubic-bezier(0.2, 0.8, 0.2, 1);
}

.menu-enter-from,
.menu-leave-to {
  opacity: 0;
  transform: translateY(-6px) scale(0.97);
}

.spacer {
  flex: 1;
}

.ghost {
  border-color: var(--fl-border);
  background: transparent;
  color: var(--fl-text-2);
}

.ghost.icon {
  width: 30px;
  padding: 5px 0;
}

/* 齿轮按钮：设置是全局入口，视觉上与主题按钮保持同级，但稍亮一点引起注意 */
.ghost.gear {
  font-size: 14px;
}

.banner {
  flex: none;
  display: flex;
  align-items: center;
  gap: 10px;
  background: var(--fl-red-soft);
  color: var(--fl-red);
  font-size: 12px;
  padding: 7px 16px;
  border-bottom: 1px solid var(--fl-border);
}

.banner-text {
  flex: 1;
}

/* 接管残留用橙色：是「需要处理」而不是「已出错」 */
.banner.warn {
  background: var(--fl-orange-soft);
  color: var(--fl-orange);
}

.banner.info {
  background: var(--fl-accent-soft);
  color: var(--fl-accent);
}

.banner .small {
  flex: none;
  font-size: 11px;
  padding: 3px 10px;
  background: transparent;
  border-color: currentColor;
  color: inherit;
}

.sysproxy {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.sysproxy .sdot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--fl-text-3, var(--fl-text-2));
  opacity: 0.5;
}

.sysproxy.on {
  background: var(--fl-green-soft);
  border-color: transparent;
  color: var(--fl-green);
}

.sysproxy.on .sdot {
  background: var(--fl-green);
  opacity: 1;
}

.content {
  flex: 1;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
  animation: fl-rise 0.55s var(--fl-ease) 0.08s both;
}

.content > * {
  flex: 1;
  min-height: 0;
}

.toast {
  position: fixed;
  left: 50%;
  bottom: 28px;
  transform: translateX(-50%);
  background: var(--fl-text);
  color: var(--fl-bg);
  padding: 8px 16px;
  border-radius: 999px;
  font-size: 12px;
  box-shadow: var(--fl-shadow);
  z-index: 60;
}

.toast.error {
  background: var(--fl-red);
  color: #fff;
}

.toast.success {
  background: var(--fl-green);
  color: #fff;
}

.toast-enter-active,
.toast-leave-active {
  transition:
    opacity 0.2s ease,
    transform 0.24s var(--fl-ease);
}

.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translate(-50%, 10px) scale(0.92);
}
</style>
