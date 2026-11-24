<script setup lang="ts">
/**
 * 设置抽屉：低频操作的统一入口，从右侧滑出覆盖在流量主页上。
 *
 * 两个分区：
 *  - 通用设置：网络代理 / 系统代理 / 解密抓取 / Map Local / 界面；
 *  - 证书与接入：原「证书与接入」页整体迁入（接入向导 + 根证书管理）。
 *
 * 为什么做成抽屉而不是独立页面：抓包是主任务，设置只是中途调一下；
 * 抽屉让用户改完配置关闭后立即回到原来的列表位置（流量页不销毁）。
 */
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { Dialogs } from '@wailsio/runtime'
import type { AppConfig, MapLocalRule, MapRemoteRule } from '../types'
import CertView from './CertView.vue'
import { useConfigStore } from '../stores/config'
import { useTrafficStore } from '../stores/traffic'
import { useUiStore } from '../stores/ui'
import { useI18n } from '../i18n'

const ui = useUiStore()
const config = useConfigStore()
const traffic = useTrafficStore()
const { t } = useI18n()

const form = reactive<AppConfig>({ ...(config.config as AppConfig) })
const dirty = ref(false)
const saving = ref(false)
const skipText = ref((config.config?.skipDecryptHosts || []).join('\n'))
const maxBodyMB = ref(Math.round((config.config?.maxBodyBytes || 2097152) / 1024 / 1024))

// 外部配置变化（例如重新拉取首屏）时同步表单，避免界面显示旧值
watch(
  () => config.config,
  (next) => {
    if (!next) return
    Object.assign(form, next)
    skipText.value = next.skipDecryptHosts.join('\n')
    maxBodyMB.value = Math.round(next.maxBodyBytes / 1024 / 1024)
    dirty.value = false
  },
)

watch(
  () => ({ ...form, skipText: skipText.value, maxBodyMB: maxBodyMB.value }),
  () => {
    dirty.value = true
  },
  { deep: true },
)

const lanMode = computed(() => form.listenHost === '0.0.0.0')

/** 开了认证却没有密码：后端会拒绝保存，这里提前把按钮禁掉并说明原因。 */
const authReady = computed(() => !form.requireAuth || form.authPassword.trim().length > 0)

/** 局域网 + 未开认证：这是最需要提醒的组合（同网段任何人都能用你的代理）。 */
const lanWithoutAuth = computed(() => lanMode.value && !form.requireAuth)

/** 生成一个便于手抄的随机密码（去掉容易混淆的字符）。 */
function generatePassword(): void {
  const alphabet = 'abcdefghjkmnpqrstuvwxyz23456789'
  const bytes = new Uint8Array(10)
  crypto.getRandomValues(bytes)
  form.authPassword = Array.from(bytes, (b) => alphabet[b % alphabet.length]).join('')
  form.requireAuth = true
}

/** 系统代理接管状态的一句话摘要 + 颜色。 */
const sysState = computed(() => {
  const s = config.sysProxy
  if (!s) return { text: t('状态未知'), cls: '' }
  if (!s.supported) return { text: t('无法自动接管'), cls: 'off' }
  if (s.stale) return { text: t('上次未正常恢复'), cls: 'warn' }
  if (s.enabled) return { text: t('已接管 → {addr}', { addr: s.addr }), cls: 'on' }
  return { text: t('未接管'), cls: 'off' }
})

/** 接管按钮是否可用：代理没跑起来时接管等于给自己断网。 */
const canTakeOver = computed(
  () => !!config.sysProxy?.supported && config.proxy.running && !config.sysProxyBusy,
)

async function submit(): Promise<void> {
  if (!authReady.value) {
    ui.showToast(t('已勾选代理认证，请先填写密码（可点「随机生成」）'), 'error')
    return
  }
  if (brokenRules.value > 0) {
    ui.showToast(t('有启用的本地映射规则还没填完匹配式或本地文件'), 'error')
    return
  }
  if (brokenRemoteRules.value > 0) {
    ui.showToast(t('有启用的远程映射规则还没填完匹配式或目标地址'), 'error')
    return
  }
  saving.value = true
  const payload: AppConfig = {
    ...form,
    skipDecryptHosts: skipText.value
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean),
    maxBodyBytes: Math.max(1, maxBodyMB.value) * 1024 * 1024,
  }
  const ok = await config.saveConfig(payload)
  saving.value = false
  if (ok) dirty.value = false
}

/* ---------------- Map Local ---------------- */

/**
 * 新增一条空规则。
 * 默认不启用：空匹配式一旦启用会命中所有请求，先让人填完再打勾更安全。
 */
function addRule(): void {
  if (!form.mapLocalRules) form.mapLocalRules = []
  form.mapLocalRules.push({ enabled: false, pattern: '', file: '', statusCode: 0, contentType: '' })
}

function removeRule(i: number): void {
  form.mapLocalRules.splice(i, 1)
}

/**
 * 选择本地文件。服务端（浏览器）模式下没有原生对话框，此时降级为手填路径，
 * 不弹错误——这类模式下用户本来就在命令行里操作，贴路径更顺手。
 */
async function pickFile(i: number): Promise<void> {
  try {
    const picked = await Dialogs.OpenFile({
      Title: t('选择用于映射的本地文件'),
      CanChooseFiles: true,
      CanChooseDirectories: false,
      AllowsMultipleSelection: false,
    })
    const path = Array.isArray(picked) ? picked[0] : picked
    if (path) form.mapLocalRules[i].file = path
  } catch {
    ui.showToast(t('当前环境无法打开文件选择框，请手动填写绝对路径'), 'error')
  }
}

/** 已启用但文件为空的规则数：提示"这条保存会失败"，避免点了保存才发现。 */
const brokenRules = computed(() =>
  (form.mapLocalRules || []).filter(
    (r: MapLocalRule) => r.enabled && (r.pattern.trim() === '' || r.file.trim() === ''),
  ).length,
)

/* ---------------- Map Remote ---------------- */

function addRemoteRule(): void {
  if (!form.mapRemoteRules) form.mapRemoteRules = []
  form.mapRemoteRules.push({ enabled: false, pattern: '', target: '' })
}

function removeRemoteRule(i: number): void {
  form.mapRemoteRules.splice(i, 1)
}

const brokenRemoteRules = computed(() =>
  (form.mapRemoteRules || []).filter(
    (r: MapRemoteRule) => r.enabled && (r.pattern.trim() === '' || r.target.trim() === ''),
  ).length,
)


async function onThemeChange(): Promise<void> {
  ui.setTheme(form.theme as 'system' | 'light' | 'dark')
  await submit()
}

async function onLocaleChange(): Promise<void> {
  ui.setLocale(form.locale as 'zh-CN' | 'en-US')
  await submit()
}

async function onStyleChange(): Promise<void> {
  ui.setStyle(form.uiStyle as 'standard' | 'spacious' | 'simple')
  await submit()
}

/** 导出配置：先走原生另存为，取消则不导出。 */
async function onExportConfig(): Promise<void> {
  await config.exportConfig(null)
}

/**
 * Esc 关闭抽屉。挂在 window 上而不是 overlay：抽屉打开时焦点通常还留在
 * 触发按钮或抽屉内的输入框里，overlay 自身永远收不到键盘事件。
 * 捕获阶段消费掉，避免同时触发别的 Esc 快捷键（如列表的"取消选择"）。
 */
function onKeydown(ev: KeyboardEvent): void {
  if (ev.key === 'Escape' && ui.settingsOpen) {
    ev.stopPropagation()
    ui.closeSettings()
  }
}

onMounted(() => window.addEventListener('keydown', onKeydown, true))
onUnmounted(() => window.removeEventListener('keydown', onKeydown, true))
</script>

<template>
  <teleport to="body">
    <transition name="drawer">
      <div v-if="ui.settingsOpen" class="overlay" @click.self="ui.closeSettings()">
        <aside class="drawer" role="dialog" aria-modal="true" :aria-label="t('设置')">
          <header class="drawer-head">
            <div class="seg" role="tablist" :aria-label="t('设置分区')">
              <button
                :class="{ active: ui.settingsSection === 'general' }"
                role="tab"
                :aria-selected="ui.settingsSection === 'general'"
                @click="ui.settingsSection = 'general'"
              >
                {{ t('通用设置') }}
              </button>
              <button
                :class="{ active: ui.settingsSection === 'cert' }"
                role="tab"
                :aria-selected="ui.settingsSection === 'cert'"
                @click="ui.settingsSection = 'cert'"
              >
                {{ t('证书与接入') }}
              </button>
            </div>
            <div class="spacer" />
            <button class="ghost close" :title="t('关闭设置（Esc）')" @click="ui.closeSettings()">✕</button>
          </header>

          <div class="drawer-body">
            <!-- 通用设置 -->
            <div v-if="ui.settingsSection === 'general'" class="settings">
              <form class="card block" @submit.prevent="submit">
                <header class="block-head">
                  <h3>{{ t('网络与代理') }}</h3>
                  <p class="muted">{{ t('修改监听地址或端口后需要重启代理。') }}</p>
                </header>

                <div class="grid">
                  <label>{{ t('监听地址') }}</label>
                  <div>
                    <select v-model="form.listenHost">
                      <option value="127.0.0.1">{{ t('仅本机（127.0.0.1）') }}</option>
                      <option value="0.0.0.0">{{ t('局域网可访问（0.0.0.0）') }}</option>
                    </select>
                    <p v-if="lanMode" class="warn-text">
                      {{ t('局域网模式下同网段设备都能连到这个代理，安全开关见下方「代理认证」。') }}
                    </p>
                  </div>

                  <label>{{ t('代理端口') }}</label>
                  <input v-model.number="form.proxyPort" type="number" min="1" max="65535" />

                  <label>{{ t('上游代理') }}</label>
                  <input
                    v-model="form.upstreamProxy"
                    :placeholder="t('http://127.0.0.1:7890（留空表示直连）')"
                  />

                  <label>{{ t('上游证书校验') }}</label>
                  <label class="inline">
                    <input v-model="form.insecureUpstream" type="checkbox" />
                    {{ t('跳过校验（仅调试自签服务时使用）') }}
                  </label>

                  <label>{{ t('代理认证') }}</label>
                  <div class="auth">
                    <label class="inline">
                      <input v-model="form.requireAuth" type="checkbox" />
                      {{ t('启用用户名密码') }}
                    </label>
                    <template v-if="form.requireAuth">
                      <input v-model="form.authUsername" :placeholder="t('用户名')" />
                      <input v-model="form.authPassword" type="password" :placeholder="t('密码（必填）')" />
                      <button type="button" class="ghost small-btn" @click="generatePassword">
                        {{ t('随机生成') }}
                      </button>
                    </template>
                    <p v-if="lanWithoutAuth" class="warn-text auth-warn">
                      {{ t('⚠️ 局域网模式下未开启认证：同网段任何人都能拿你的电脑当代理用，也能看到抓到的明文。') }}
                    </p>
                    <p v-else-if="form.requireAuth" class="muted hint auth-warn">
                      {{ t('开启后客户端必须带凭据，否则返回 407。改动立即生效，无需重启代理。') }}
                    </p>
                  </div>
                </div>
              </form>

              <section class="card block">
                <header class="block-head">
                  <h3>{{ t('系统代理') }}</h3>
                  <p class="muted">
                    {{ t('接管后本机应用的流量都会经过 观流；停止代理或退出应用时自动恢复原设置。') }}
                  </p>
                </header>

                <div class="sys-row">
                  <div class="sys-state">
                    <i class="dot" :class="sysState.cls" />
                    <div class="sys-text">
                      <div class="sys-title">{{ sysState.text }}</div>
                      <div class="muted small">{{ config.sysProxy?.detail || '—' }}</div>
                    </div>
                  </div>

                  <button
                    v-if="config.sysProxy?.supported"
                    :class="config.sysProxy?.enabled ? 'ghost' : 'primary'"
                    :disabled="config.sysProxy?.enabled ? config.sysProxyBusy : !canTakeOver"
                    @click="config.toggleSysProxy()"
                  >
                    {{ config.sysProxy?.enabled ? t('恢复原设置') : t('接管系统代理') }}
                  </button>
                </div>

                <p v-if="config.sysProxy?.supported && !config.proxy.running" class="hint">
                  {{ t('需先启动代理：否则系统会指向一个没人监听的端口，等于切断网络。') }}
                </p>
                <p v-if="config.sysProxy?.needsAdmin" class="hint">
                  {{ t('修改系统网络设置时系统会弹出授权框；密码由系统收集，观流 既不接触也不保存。') }}
                </p>
                <p class="hint">
                  {{ t('机制：{mechanism}；本机回环（localhost / 127.0.0.1 / ::1） 与内网地址已加入绕过列表。', { mechanism: config.sysProxy?.mechanism || '—' }) }}
                </p>

                <details
                  v-if="config.sysProxy && !config.sysProxy.supported && config.sysProxy.manual?.length"
                  class="manual"
                >
                  <summary>{{ t('手动配置步骤') }}</summary>
                  <ol>
                    <li v-for="step in config.sysProxy.manual" :key="step">{{ step }}</li>
                  </ol>
                </details>
              </section>

              <form class="card block" @submit.prevent="submit">
                <header class="block-head">
                  <h3>{{ t('解密与抓取') }}</h3>
                  <p class="muted">{{ t('不解密的域名会走隧道，列表中标为「未解密」。') }}</p>
                </header>

                <div class="grid">
                  <label>{{ t('HTTPS 解密') }}</label>
                  <label class="inline">
                    <input v-model="form.interceptTls" type="checkbox" />
                    {{ t('开启（关闭后仅能看到 CONNECT 隧道，看不到明文）') }}
                  </label>

                  <label>{{ t('跳过解密的域名') }}</label>
                  <div>
                    <textarea v-model="skipText" rows="4" :placeholder="t('每行一个，例如 bank.com')"></textarea>
                    <p class="muted hint">{{ t('匹配规则：域名本身或其子域。适合放行银行、支付类流量。') }}</p>
                  </div>

                  <label>{{ t('最多保留会话') }}</label>
                  <input v-model.number="form.maxFlows" type="number" min="100" max="100000" />

                  <label>{{ t('单条 Body 上限') }}</label>
                  <div class="inline">
                    <input v-model.number="maxBodyMB" type="number" min="1" max="32" class="narrow" />
                    <span class="muted">{{ t('MB，超出部分截断') }}</span>
                  </div>
                </div>
              </form>

              <form class="card block" @submit.prevent="submit">
                <header class="block-head">
                  <h3>{{ t('本地映射（Map Local）') }}</h3>
                  <p class="muted">
                    {{ t('命中的请求用本地文件内容替换响应，请求仍会真实发出。改文件即时生效，不需要重启代理。') }}
                  </p>
                </header>

                <p v-if="!form.mapLocalRules || form.mapLocalRules.length === 0" class="muted small">
                  {{ t('还没有规则。联调后端未就绪的接口时，可以把某个 URL 指向一份 JSON 文件。') }}
                </p>

                <div v-else class="rules">
                  <div
                    v-for="(r, i) in form.mapLocalRules"
                    :key="i"
                    class="rule"
                    :class="{ off: !r.enabled }"
                  >
                    <div class="rule-line">
                      <label class="inline rule-toggle">
                        <input v-model="r.enabled" type="checkbox" />
                        {{ t('启用') }}
                      </label>
                      <input
                        v-model="r.pattern"
                        class="mono"
                        spellcheck="false"
                        :placeholder="t('匹配式：api/users ｜ https://a.com/api/* ｜ re:^https://a\\\\.com/v\\\\d+')"
                      />
                      <button type="button" class="ghost small-btn" :title="t('删除该规则')" @click="removeRule(i)">
                        {{ t('删除') }}
                      </button>
                    </div>
                    <div class="rule-line">
                      <input
                        v-model="r.file"
                        class="mono"
                        spellcheck="false"
                        :placeholder="t('本地文件绝对路径，如 /Users/you/mock/users.json')"
                      />
                      <button type="button" class="ghost small-btn" @click="pickFile(i)">{{ t('选择…') }}</button>
                    </div>
                    <div class="rule-line rule-extra">
                      <span class="muted small">{{ t('状态码') }}</span>
                      <input
                        v-model.number="r.statusCode"
                        type="number"
                        min="0"
                        max="599"
                        class="tiny"
                        placeholder="0"
                      />
                      <span class="muted small">Content-Type</span>
                      <input v-model="r.contentType" class="mono ct" :placeholder="t('留空按扩展名推断')" />
                    </div>
                  </div>
                </div>

                <div class="rules-foot">
                  <button type="button" class="ghost" @click="addRule">+ {{ t('添加规则') }}</button>
                  <span class="muted small">
                    {{ t('按顺序匹配，第一条命中生效；状态码 0 表示沿用上游。响应体过大（超过流式阈值）时不生效。') }}
                  </span>
                </div>
                <p v-if="brokenRules > 0" class="warn-text">
                  {{ t('有 {count} 条已启用的规则还没填完（匹配式或文件为空），保存会被拒绝。', { count: brokenRules }) }}
                </p>
              </form>

              <form class="card block" @submit.prevent="submit">
                <header class="block-head">
                  <h3>{{ t('远程映射（Map Remote）') }}</h3>
                  <p class="muted">
                    {{ t('命中的请求被改写到目标地址后真实发出，响应来自新目标。') }}
                    {{ t('目标只写源（https://new.com）时保留原路径；带路径（https://new.com/v2）时作为路径前缀拼接。') }}
                  </p>
                </header>

                <p v-if="!form.mapRemoteRules || form.mapRemoteRules.length === 0" class="muted small">
                  {{ t('还没有规则。典型用法：把 api.prod.com 的流量整体切到测试环境。') }}
                </p>

                <div v-else class="rules">
                  <div
                    v-for="(r, i) in form.mapRemoteRules"
                    :key="i"
                    class="rule"
                    :class="{ off: !r.enabled }"
                  >
                    <div class="rule-line">
                      <label class="inline rule-toggle">
                        <input v-model="r.enabled" type="checkbox" />
                        {{ t('启用') }}
                      </label>
                      <input
                        v-model="r.pattern"
                        class="mono"
                        spellcheck="false"
                        :placeholder="t('匹配式（与 Map Local 同语法）：api.prod.com ｜ https://a.com/api/* ｜ re:…')"
                      />
                      <button type="button" class="ghost small-btn" :title="t('删除该规则')" @click="removeRemoteRule(i)">
                        {{ t('删除') }}
                      </button>
                    </div>
                    <div class="rule-line">
                      <input
                        v-model="r.target"
                        class="mono"
                        spellcheck="false"
                        :placeholder="t('目标地址：https://api.beta.com:8443（保留路径）或 https://new.com/v2（拼路径前缀）')"
                      />
                    </div>
                  </div>
                </div>

                <div class="rules-foot">
                  <button type="button" class="ghost" @click="addRemoteRule">+ {{ t('添加规则') }}</button>
                  <span class="muted small">
                    {{ t('按顺序匹配，第一条命中生效。本地映射与远程映射同用一套匹配语法；两者同时命中时先改写请求（Remote），再替换响应（Local）。') }}
                  </span>
                </div>
                <p v-if="brokenRemoteRules > 0" class="warn-text">
                  {{ t('有 {count} 条已启用的远程映射还没填完（匹配式或目标为空），保存会被拒绝。', { count: brokenRemoteRules }) }}
                </p>
              </form>

              <form class="card block" @submit.prevent="submit">
                <header class="block-head">
                  <h3>{{ t('断点') }}</h3>
                  <p class="muted">
                    {{ t('命中的请求或响应会暂停并弹出编辑窗口，放行前客户端一直在等。') }}
                    {{ t('忘了处置也没关系：超过设定时间自动放行原内容。') }}
                  </p>
                </header>
                <div class="grid">
                  <label>{{ t('启用断点') }}</label>
                  <label class="inline">
                    <input v-model="form.breakpointEnabled" type="checkbox" />
                    {{ t('开启（改完保存后立即生效，无需重启代理）') }}
                  </label>

                  <label>{{ t('拦截阶段') }}</label>
                  <div class="inline wrap">
                    <label class="inline">
                      <input v-model="form.breakpointRequest" type="checkbox" :disabled="!form.breakpointEnabled" />
                      {{ t('请求（可改方法 / URL / 头 / 体）') }}
                    </label>
                    <label class="inline">
                      <input v-model="form.breakpointResponse" type="checkbox" :disabled="!form.breakpointEnabled" />
                      {{ t('响应（可改状态码 / 头 / 体）') }}
                    </label>
                  </div>

                  <label>{{ t('自动放行超时') }}</label>
                  <div class="inline">
                    <input
                      v-model.number="form.breakpointTimeout"
                      type="number"
                      min="5"
                      max="600"
                      class="narrow"
                      :disabled="!form.breakpointEnabled"
                    />
                    <span class="muted">{{ t('秒（5-600）') }}</span>
                  </div>
                </div>
                <p v-if="form.breakpointEnabled && !form.breakpointRequest && !form.breakpointResponse" class="warn-text">
                  {{ t('已开启断点但没有勾选任何阶段，保存后会按「仅拦截请求」生效。') }}
                </p>
              </form>

              <form class="card block" @submit.prevent="submit">
                <header class="block-head">
                  <h3>{{ t('界面') }}</h3>
                </header>
                <div class="grid">
                  <label>{{ t('主题') }}</label>
                  <select v-model="form.theme" @change="onThemeChange">
                    <option value="system">{{ t('跟随系统') }}</option>
                    <option value="light">{{ t('浅色') }}</option>
                    <option value="dark">{{ t('深色') }}</option>
                  </select>
                  <label>{{ t('语言') }}</label>
                  <select v-model="form.locale" @change="onLocaleChange">
                    <option value="zh-CN">{{ t('中文（简体）') }}</option>
                    <option value="en-US">English</option>
                  </select>
                  <label>{{ t('界面风格') }}</label>
                  <select v-model="form.uiStyle" @change="onStyleChange">
                    <option value="standard">{{ t('标准') }}</option>
                    <option value="spacious">{{ t('舒展（大屏）') }}</option>
                    <option value="simple">{{ t('极简') }}</option>
                  </select>
                </div>
              </form>

              <!-- 极简风格下过滤条只剩搜索，导出/清空入口收在这里（功能不删） -->
              <form v-if="ui.style === 'simple'" class="card block">
                <header class="block-head">
                  <h3>{{ t('会话管理') }}</h3>
                </header>
                <div class="simple-tools">
                  <button type="button" :disabled="traffic.flows.length === 0" @click="traffic.exportHAR([])">
                    {{ t('导出 HAR') }}
                  </button>
                  <button type="button" :disabled="traffic.flows.length === 0" @click="traffic.exportJSON([])">
                    {{ t('导出 JSON') }}
                  </button>
                  <button
                    type="button"
                    class="ghost"
                    :disabled="traffic.flows.length === 0"
                    @click="traffic.clearFlows()"
                  >
                    {{ t('清空') }}
                  </button>
                </div>
              </form>

              <div class="footer">
                <span class="muted path">{{ t('配置文件：{path}', { path: config.bootstrap?.configPath ?? '' }) }}</span>
                <span class="muted path">v{{ config.bootstrap?.version || '0.1.0' }}</span>
                <div class="spacer" />
                <button type="button" class="ghost" :title="t('导出当前全部配置为 JSON 文件')" @click="onExportConfig">
                  {{ t('导出配置') }}
                </button>
                <button type="button" class="ghost" :title="t('从 JSON 文件导入配置并立即生效')" @click="config.importConfig()">
                  {{ t('导入配置') }}
                </button>
                <button v-if="config.proxy.running" class="ghost" @click="config.restartProxy()">
                  {{ t('重启代理') }}
                </button>
                <button
                  class="primary"
                  :disabled="saving || !dirty || !authReady || brokenRules > 0 || brokenRemoteRules > 0"
                  @click="submit"
                >
                  {{ saving ? t('保存中…') : t('保存配置') }}
                </button>
              </div>
            </div>

            <!-- 证书与接入：整体复用原视图 -->
            <CertView v-else />
          </div>
        </aside>
      </div>
    </transition>
  </teleport>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 40;
  background: color-mix(in srgb, var(--fl-text) 28%, transparent);
  -webkit-backdrop-filter: blur(3px);
  backdrop-filter: blur(3px);
  display: flex;
  justify-content: flex-end;
}

.drawer {
  width: min(760px, 92vw);
  height: 100%;
  background: var(--fl-bg);
  border-left: 1px solid var(--fl-border);
  box-shadow: var(--fl-shadow);
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.drawer-head {
  flex: none;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--fl-border);
  background: var(--fl-panel);
}

.seg {
  display: inline-flex;
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  overflow: hidden;
}

.seg button {
  border: none;
  background: transparent;
  color: var(--fl-text-2);
  font-size: 12px;
  padding: 5px 12px;
}

.seg button + button {
  border-left: 1px solid var(--fl-border);
}

.seg button.active {
  background: var(--fl-accent-soft);
  color: var(--fl-accent);
  font-weight: 600;
}

.spacer {
  flex: 1;
}

.close {
  width: 28px;
  padding: 4px 0;
  font-size: 12px;
}

.ghost {
  border-color: var(--fl-border);
  background: transparent;
  color: var(--fl-text-2);
}

.drawer-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
}

.settings {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.block {
  padding: 16px 18px;
}

.block-head {
  margin-bottom: 14px;
}

h3 {
  margin: 0 0 2px;
  font-size: 14px;
}

.block-head p {
  margin: 0;
  font-size: 12px;
}

.grid {
  display: grid;
  grid-template-columns: 130px 1fr;
  gap: 12px 16px;
  align-items: start;
}

.grid > label:first-child {
  color: var(--fl-text-2);
  padding-top: 5px;
}

input[type="number"],
input:not([type]),
textarea,
select {
  width: 100%;
  max-width: 380px;
}

textarea {
  font: inherit;
  color: var(--fl-text);
  background: var(--fl-panel);
  border: 1px solid var(--fl-border-strong);
  border-radius: var(--fl-radius-sm);
  padding: 8px 10px;
  resize: vertical;
  outline: none;
}

textarea:focus {
  border-color: var(--fl-accent);
  box-shadow: 0 0 0 3px var(--fl-accent-soft);
}

.inline {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--fl-text-2);
}

/* 允许换行的复选框组（断点的两个阶段选项） */
.inline.wrap {
  flex-wrap: wrap;
}

.narrow {
  width: 80px;
}

.auth {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.auth input:not([type="checkbox"]) {
  max-width: 140px;
}

.auth-warn {
  flex-basis: 100%;
}

.small-btn {
  font-size: 11px;
  padding: 3px 8px;
}

.hint {
  margin: 6px 0 0;
  font-size: 11px;
}

.warn-text {
  margin: 6px 0 0;
  font-size: 12px;
  color: var(--fl-orange);
}

.footer {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 2px 12px;
}

.path {
  font-size: 11px;
  word-break: break-all;
}

.rules {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.rule {
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  padding: 8px 10px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  background: var(--fl-panel-2);
}

/* 未启用的规则整体降饱和，一眼能看出"这条现在不生效" */
.rule.off {
  opacity: 0.66;
}

.rule-line {
  display: flex;
  align-items: center;
  gap: 8px;
}

.rule-line input.mono {
  flex: 1;
  min-width: 0;
  max-width: none;
}

.rule-toggle {
  flex: none;
  width: 62px;
}

.rule-extra input.tiny {
  width: 72px;
  flex: none;
}

.rule-extra input.ct {
  flex: 1;
  min-width: 0;
  max-width: 260px;
}

.rules-foot {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 10px;
}

.sys-row {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.sys-state {
  flex: 1;
  min-width: 220px;
  display: flex;
  align-items: center;
  gap: 10px;
}

.sys-state .dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex: none;
  background: var(--fl-text-2);
  opacity: 0.4;
}

.sys-state .dot.on {
  background: var(--fl-green);
  opacity: 1;
}

.sys-state .dot.warn {
  background: var(--fl-orange);
  opacity: 1;
}

.sys-state .dot.off {
  opacity: 0.4;
}

.sys-title {
  font-size: 13px;
  margin-bottom: 2px;
}

.small {
  font-size: 11px;
}

.manual {
  margin-top: 12px;
  font-size: 12px;
  color: var(--fl-text-2);
}

.manual summary {
  cursor: pointer;
  color: var(--fl-accent);
}

.manual ol {
  margin: 8px 0 0;
  padding-left: 20px;
  line-height: 1.7;
}

/* 抽屉进出场：右滑入 / 右滑出，遮罩同步淡入淡出 */
.drawer-enter-active,
.drawer-leave-active {
  transition: opacity 0.22s ease;
}

.drawer-enter-active .drawer,
.drawer-leave-active .drawer {
  transition: transform 0.34s var(--fl-ease);
}

/* 卡片随抽屉打开依次浮现：低频设置页的仪式感，不干扰高频列表 */
.drawer-enter-active .settings > .card {
  animation: fl-rise 0.36s var(--fl-ease) both;
}

.drawer-enter-active .settings > .card:nth-child(2) {
  animation-delay: 0.03s;
}
.drawer-enter-active .settings > .card:nth-child(3) {
  animation-delay: 0.06s;
}
.drawer-enter-active .settings > .card:nth-child(4) {
  animation-delay: 0.09s;
}
.drawer-enter-active .settings > .card:nth-child(n + 5) {
  animation-delay: 0.12s;
}

.drawer-enter-from,
.drawer-leave-to {
  opacity: 0;
}

.drawer-enter-from .drawer,
.drawer-leave-to .drawer {
  transform: translateX(24px);
}

.simple-tools {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
</style>
