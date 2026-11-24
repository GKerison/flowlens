<script setup lang="ts">
/**
 * 证书与接入页 = 接入向导。
 *
 * 四步分别对应一个真实门槛，任一步没做，用户就会"看不到明文"，且往往不知道卡在哪：
 *   1. 代理没启动 → 连不上；
 *   2. 本机没信任证书 → 浏览器直接拦截；
 *   3. 手机没连上/没装证书 → 手机流量抓不到；
 *   4. 以上都对但抓取暂停 → 列表空白。
 * 因此每一步都给出"当前状态 + 一个可点的动作 + 失败时怎么办"。
 *
 * 安全约定：安装/卸载系统信任属于敏感操作，
 * 必须先把将执行的命令展示出来（可折叠），用户点确认后才调用后端。
 */
import { computed, onMounted, ref, watch } from 'vue'
import QRCode from 'qrcode'
import { useConfigStore } from '../stores/config'
import { useUiStore } from '../stores/ui'
import { useI18n } from '../i18n'

const config = useConfigStore()
const ui = useUiStore()
const { t } = useI18n()

const showPEM = ref(false)

/* ---------------- 导出 .p12 ---------------- */

/**
 * 只有在用户显式勾选"我知道私钥泄露的后果"后才允许导出。
 * 这不是仪式感：p12 里是能签发任意站点证书的私钥，
 * 一旦流到别处，你抓到过的所有 HTTPS 流量都可能被解密。
 */
const p12Open = ref(false)
const p12Password = ref('')
const p12Ack = ref(false)
const p12Busy = ref(false)
const p12Path = ref('')

const p12Ready = computed(
  () => p12Ack.value && p12Password.value.length >= 8 && !p12Busy.value,
)

async function onExportP12(): Promise<void> {
  if (!p12Ready.value) return
  p12Busy.value = true
  p12Path.value = ''
  try {
    const res = await config.exportCA(p12Password.value)
    p12Path.value = res.path
    ui.showToast(t('已导出 p12，请立即转移到安全位置'), 'success')
    // 导出后清掉密码，不留在内存/界面上
    p12Password.value = ''
    p12Ack.value = false
  } catch (err) {
    ui.showToast(err instanceof Error ? err.message : String(err), 'error')
  } finally {
    p12Busy.value = false
  }
}
const showCommand = ref(false)
/** 用户级安装失败后，才引导他改用系统级（需要管理员密码） */
const installFailed = ref(false)
/** 手机填代理时要看的密码：默认遮住，避免投屏/截图时泄露 */
const showPass = ref(false)

const ca = computed(() => config.ca)
const trust = computed(() => config.trust)
const cert = computed(() => config.cert)
const proxy = computed(() => config.proxy)

/** 每一步是否已完成，用于顶部进度与卡片角标。 */
const done = computed(() => ({
  proxy: !!proxy.value.running,
  trust: !!trust.value?.trusted,
  phone: !!cert.value?.running && !!cert.value?.lan,
  verify: !!config.selfTest?.ok,
}))

const validity = computed(() => {
  if (!ca.value) return ''
  const from = new Date(ca.value.notBefore).toLocaleDateString('zh-CN')
  const to = new Date(ca.value.notAfter).toLocaleDateString('zh-CN')
  return t('{from} 至 {to}', { from, to })
})

const daysLeft = computed(() => {
  if (!ca.value) return 0
  const ms = new Date(ca.value.notAfter).getTime() - Date.now()
  return Math.max(0, Math.floor(ms / 86400000))
})

/** 当前平台信任库名称，避免一律说"钥匙串"。 */
const storeName = computed(() => trust.value?.store?.name || t('系统信任库'))

/** 二维码内容：优先局域网地址，手机扫码才有意义。 */
const qrURL = computed(() => cert.value?.url || '')

/** 是否开启了代理认证：开启后手机填代理必须同时填用户名密码。 */
const authOn = computed(() => !!config.config?.requireAuth && !!config.config?.authPassword)

/** 手机要填的代理地址：局域网模式下给电脑的局域网 IP，否则退回本机地址。 */
const phoneProxyAddr = computed(() => {
  const cfg = config.config
  const ips = config.bootstrap?.lanIps || []
  if (cfg?.listenHost === '0.0.0.0' && ips.length > 0) {
    return `http://${ips[0]}:${cfg.proxyPort}`
  }
  return config.bootstrap?.proxyUrl || '—'
})

const qrCanvas = ref<HTMLCanvasElement | null>(null)
const qrError = ref('')

/** 渲染二维码。地址变化、主题切换都会重算，保证扫码内容与界面一致。 */
async function renderQR(): Promise<void> {
  const el = qrCanvas.value
  const url = qrURL.value
  if (!el || !url) return
  try {
    await QRCode.toCanvas(el, url, {
      width: 224,
      margin: 1,
      errorCorrectionLevel: 'M',
      // 固定深色前景 + 白底：暗色主题下反色会导致部分机型扫不出来
      color: { dark: '#12141a', light: '#ffffff' },
    })
    qrError.value = ''
  } catch {
    qrError.value = t('二维码生成失败，请手动在手机上输入地址')
  }
}

watch([qrURL, qrCanvas], () => void renderQR(), { flush: 'post' })
watch(() => ui.theme, () => void renderQR(), { flush: 'post' })

onMounted(async () => {
  // 分区打开即刷新：可能在其他界面启动过代理、改过端口
  await Promise.all([config.refreshTrust(), config.refreshCert()])
  await renderQR()
})

/** 安装信任。失败时把"改用系统级"的入口亮出来，而不是让用户反复点同一个按钮。 */
async function onInstall(elevated = false): Promise<void> {
  installFailed.value = false
  const ok = await config.installTrust(elevated)
  installFailed.value = !ok
}

/** 引导改监听地址：切到设置抽屉的「通用设置」分区。 */
function goSettings(): void {
  ui.openSettings('general')
}
</script>

<template>
  <div class="cert">
    <div class="cert-body">
    <!-- 概览 -->
    <section class="card intro">
      <div class="intro-text">
        <h3>{{ t('接入向导') }}</h3>
        <p class="muted">
          {{ t('四步跑通：启动代理 → 本机信任 → 手机扫码 → 自检。做完第 ④ 步能看到明文，说明整条链路都通了。') }}
        </p>
      </div>
      <ol class="progress">
        <li :class="{ ok: done.proxy }">{{ t('① 代理') }}</li>
        <li :class="{ ok: done.trust }">{{ t('② 信任') }}</li>
        <li :class="{ ok: done.phone }">{{ t('③ 手机') }}</li>
        <li :class="{ ok: done.verify }">{{ t('④ 自检') }}</li>
      </ol>
    </section>

    <!-- ① 启动代理 -->
    <section class="card step" :class="{ ok: done.proxy }">
      <header class="step-head">
        <span class="no">1</span>
        <div class="grow">
          <h3>{{ t('启动代理') }}</h3>
          <p class="muted">{{ t('代理在跑，客户端才有地方转发流量。') }}</p>
        </div>
        <span class="badge" :class="done.proxy ? 'ok' : 'todo'">
          {{ done.proxy ? t('运行中') : t('未启动') }}
        </span>
      </header>

      <div class="row">
        <code class="addr">{{ config.bootstrap?.proxyUrl || '—' }}</code>
        <button v-if="!done.proxy" class="primary" @click="config.toggleProxy()">{{ t('启动代理') }}</button>
        <button v-else class="ghost" @click="config.toggleProxy()">{{ t('停止代理') }}</button>
      </div>

      <dl class="kv">
        <dt>{{ t('监听地址') }}</dt>
        <dd>
          <code>{{ config.config?.listenHost || '—' }}</code>
          <span v-if="cert?.lan" class="muted">{{ t('（局域网可访问）') }}</span>
          <span v-else class="warn-text">{{ t('（仅本机可访问，手机连不上）') }}</span>
        </dd>
        <dt>{{ t('暂停状态') }}</dt>
        <dd>
          <span v-if="proxy.paused" class="warn-text">{{ t('已暂停：当前仅作中转，不解密不抓包，上网不受影响') }}</span>
          <span v-else class="muted">{{ t('正常抓取') }}</span>
        </dd>
      </dl>

      <p class="muted small">
        {{ t('顶栏的按钮用于启停代理；本页只负责检查状态。') }}
        <template v-if="!cert?.lan">
          {{ t('手机接入前请把监听地址改为') }} <code>0.0.0.0</code>。
          <button class="link" @click="goSettings">{{ t('去设置修改') }}</button>
        </template>
        <template v-else>{{ t('当前已对局域网开放，手机可以直接连。') }}</template>
      </p>
    </section>

    <!-- ② 本机信任 -->
    <section class="card step" :class="{ ok: done.trust }">
      <header class="step-head">
        <span class="no">2</span>
        <div class="grow">
          <h3>{{ t('本机信任根证书') }}</h3>
          <p class="muted">{{ t('不装这一步，浏览器会报「证书不受信任」，抓不到明文。') }}</p>
        </div>
        <span class="badge" :class="done.trust ? 'ok' : 'todo'">
          {{ done.trust ? (trust?.elevated ? t('已信任（系统级）') : t('已信任（用户级）')) : t('未安装') }}
        </span>
      </header>

      <p class="status-line">
        <span class="dot" :class="done.trust ? 'ok' : 'warn'" />
        {{ trust?.detail || t('正在读取信任状态…') }}
      </p>

      <dl class="kv">
        <dt>{{ t('安装到') }}</dt>
        <dd>{{ storeName }}<span v-if="trust?.store?.requires" class="muted">，{{ trust.store.requires }}</span></dd>
        <dt>{{ t('证书指纹') }}</dt>
        <dd class="mono break small">{{ ca?.fingerprint }}</dd>
      </dl>

      <div class="actions">
        <button
          v-if="!done.trust"
          class="primary"
          :disabled="config.trustBusy"
          @click="onInstall(false)"
        >
          {{ config.trustBusy ? t('安装中…') : t('一键安装到系统信任') }}
        </button>
        <button
          v-if="!done.trust && installFailed && trust?.canElevate"
          :disabled="config.trustBusy"
          @click="onInstall(true)"
        >
          {{ t('以管理员身份重试') }}
        </button>
        <button v-if="done.trust" class="danger" :disabled="config.trustBusy" @click="config.uninstallTrust()">
          {{ t('卸载信任') }}
        </button>
        <button class="ghost" :disabled="config.trustBusy" @click="config.refreshTrust()">{{ t('重新检测') }}</button>
      </div>

      <p v-if="installFailed" class="tip warn-box">
        {{ t('安装未成功。可以换个位置重试（上方的「以管理员身份重试」），或按下面的命令手动执行。') }}
      </p>

      <!-- 命令预览：让"一键"不神秘 -->
      <button class="disclose" @click="showCommand = !showCommand">
        {{ showCommand ? '▾' : '▸' }} {{ t('将执行的命令') }}
      </button>
      <pre v-if="showCommand" class="cmd">{{ (trust?.command || []).join(' ') || '—' }}</pre>
    </section>

    <!-- ③ 手机接入 -->
    <section class="card step" :class="{ ok: done.phone }">
      <header class="step-head">
        <span class="no">3</span>
        <div class="grow">
          <h3>{{ t('手机 / 其他设备接入') }}</h3>
          <p class="muted">{{ t('手机与电脑连同一个 Wi-Fi，扫码打开下载页，装证书即可。') }}</p>
        </div>
        <span class="badge" :class="done.phone ? 'ok' : 'todo'">
          {{ cert?.running ? (cert.lan ? t('可扫码') : t('仅本机')) : t('服务未运行') }}
        </span>
      </header>

      <div v-if="cert?.running && qrURL" class="phone">
        <div class="qr-box">
          <canvas ref="qrCanvas" class="qr" />
          <p v-if="qrError" class="warn-text small">{{ qrError }}</p>
        </div>
        <div class="grow">
          <p class="muted small">{{ t('手机扫码打开下载页：') }}</p>
          <code class="addr">{{ qrURL }}</code>
          <div class="actions">
            <button class="ghost" @click="ui.copyText(qrURL, t('地址已复制'))">{{ t('复制地址') }}</button>
            <button class="ghost" @click="config.refreshCert()">{{ t('刷新') }}</button>
          </div>
          <p class="muted small">
            {{ t('扫码后按页面指引安装：iPhone 装完描述文件还需去「设置 → 通用 → 关于本机 → 证书信任设置」打开开关，否则 Safari 会提示无法验证服务器身份。') }}
          </p>
        </div>
      </div>

      <div v-else class="empty-box">
        <p class="warn-text">
          {{ cert?.error || t('证书服务未运行，手机无法下载证书。') }}
        </p>
        <button @click="config.restartCertServer()">{{ t('重启证书服务') }}</button>
      </div>

      <p v-if="cert?.running && !cert.lan" class="tip warn-box">
        {{ t('证书服务当前只监听本机，手机访问不到。请到') }}
        <button class="link" @click="goSettings">{{ t('设置') }}</button>
        {{ t('把「监听地址」改为') }} <code>0.0.0.0</code>{{ t('（保存后地址会自动刷新）。') }}
      </p>

      <dl class="kv">
        <dt>{{ t('手机填代理') }}</dt>
        <dd class="mono small break">{{ phoneProxyAddr }}</dd>
        <template v-if="authOn">
          <dt>{{ t('认证') }}</dt>
          <dd class="small">
            {{ t('用户名') }} <code>{{ config.config?.authUsername }}</code>{{ t('，密码') }}
            <code>{{ showPass ? config.config?.authPassword : '••••••' }}</code>
            <button class="link" @click="showPass = !showPass">
              {{ showPass ? t('隐藏') : t('显示') }}
            </button>
            <span class="muted">{{ t('（代理已开启认证，手机端必须填这两项，否则表现为「连不上网」）') }}</span>
          </dd>
        </template>
        <dt>{{ t('其他地址') }}</dt>
        <dd class="mono small break">{{ (cert?.urls || []).join('　') || '—' }}</dd>
        <dt>{{ t('端口') }}</dt>
        <dd class="mono small">{{ cert?.port }}{{ t('（可在设置中修改）') }}</dd>
      </dl>

      <p class="muted small">
        {{ t('也可以手动把根证书装到电脑：访问') }} <code>{{ cert?.url || '—' }}</code> {{ t('下载') }}
        <code>.crt</code> {{ t('或 iOS 描述文件。') }}
      </p>
    </section>

    <!-- ④ 自检 -->
    <section class="card step" :class="{ ok: done.verify }">
      <header class="step-head">
        <span class="no">4</span>
        <div class="grow">
          <h3>{{ t('自检') }}</h3>
          <p class="muted">{{ t('通过代理访问一次 HTTPS，验证「解密 + 证书信任」整条链路。') }}</p>
        </div>
        <span class="badge" :class="done.verify ? 'ok' : 'todo'">
          {{ config.selfTestBusy ? t('检测中') : done.verify ? t('通过') : t('未通过') }}
        </span>
      </header>

      <div class="actions">
        <button class="primary" :disabled="config.selfTestBusy" @click="config.runSelfTest()">
          {{ config.selfTestBusy ? t('检测中…') : t('运行自检') }}
        </button>
      </div>

      <div v-if="config.selfTest" class="result" :class="config.selfTest.ok ? 'ok' : 'bad'">
        <p class="result-title">{{ config.selfTest.ok ? t('✓ 链路正常') : t('✕ 未能通过') }}</p>
        <p class="result-msg">{{ config.selfTest.message }}</p>
        <p v-if="config.selfTest.hint" class="muted small">{{ t('下一步：') }}{{ config.selfTest.hint }}</p>
        <p class="muted small">
          {{ t('目标 {target} · 状态 {status} · {duration} ms', { target: config.selfTest.targetUrl || '—', status: config.selfTest.status || '—', duration: config.selfTest.durationMs }) }}
        </p>
      </div>
    </section>

    <!-- 根证书（高级） -->
    <section class="card">
      <header class="step-head">
        <div class="grow">
          <h3>{{ t('根证书') }}</h3>
          <p class="muted">{{ t('观流 自签，只保存在本机；重新生成后所有设备都要重装。') }}</p>
        </div>
        <span class="badge" :class="daysLeft > 30 ? 'ok' : 'warn'">
          {{ daysLeft > 30 ? t('有效') : t('即将过期') }}
        </span>
      </header>

      <dl v-if="ca" class="kv">
        <dt>{{ t('名称') }}</dt>
        <dd>{{ ca.commonName }}</dd>
        <dt>{{ t('有效期') }}</dt>
        <dd>{{ validity }} <span class="muted">{{ t('（剩余 {days} 天）', { days: daysLeft }) }}</span></dd>
        <dt>SHA-256</dt>
        <dd class="mono break small">
          {{ ca.fingerprint }}
          <button class="ghost tiny" @click="ui.copyText(ca.fingerprint, t('指纹已复制'))">{{ t('复制') }}</button>
        </dd>
        <dt>{{ t('存储路径') }}</dt>
        <dd class="mono small break">{{ ca.path }}</dd>
      </dl>

      <div class="actions">
        <button class="ghost" @click="showPEM = !showPEM">{{ showPEM ? t('隐藏') : t('查看') }} PEM</button>
        <button class="ghost" @click="ui.copyText(ca?.pem || '', t('证书内容已复制'))">{{ t('复制 PEM') }}</button>
        <button class="danger" @click="config.regenerateCA()">{{ t('重新生成') }}</button>
      </div>

      <pre v-if="showPEM" class="cmd">{{ ca?.pem }}</pre>

      <div class="p12">
        <div class="p12-head">
          <strong>{{ t('导出 .p12（含私钥）') }}</strong>
          <span class="badge warn">{{ t('高风险') }}</span>
          <button class="ghost tiny" @click="p12Open = !p12Open">
            {{ p12Open ? t('收起') : t('展开') }}
          </button>
        </div>
        <p class="muted small">
          {{ t('少数场景确实需要它：Java keystore、Android 系统级证书、只接受 p12 的企业客户端。') }}
          {{ t('普通抓包') }}<b>{{ t('不需要') }}</b>{{ t('导出私钥，装 PEM 就够。') }}
        </p>

        <div v-if="p12Open" class="p12-body">
          <p class="danger-text">
            {{ t('⚠️ 文件里是根证书私钥：拿到它的人可以解密你之后抓到的任何 HTTPS 流量，也能冒充任意网站。') }}
            {{ t('请只放在你自己控制的位置，不要发到聊天工具、不要进代码仓库、用完尽快删除。') }}
          </p>
          <label class="inline">
            <input v-model="p12Ack" type="checkbox" />
            {{ t('我知道泄露私钥的后果，且不会把文件放在共享位置') }}
          </label>
          <div class="p12-row">
            <input
              v-model="p12Password"
              type="password"
              :placeholder="t('设置导出密码（至少 8 位）')"
              autocomplete="new-password"
            />
            <button class="danger" :disabled="!p12Ready" @click="onExportP12">
              {{ p12Busy ? t('导出中…') : t('导出到下载目录') }}
            </button>
          </div>
          <p class="muted small">
            {{ t('文件以 0600 权限写入；导入时（如') }} <code>keytool -importkeystore</code>、<code>security import</code>{{ t('）需要同一个密码。') }}
          </p>
          <p v-if="p12Path" class="mono small break">{{ t('已导出：') }}{{ p12Path }}</p>
        </div>
      </div>
    </section>
    </div>
  </div>
</template>

<style scoped>
.inline {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--fl-text-2);
  font-size: 12px;
}

.p12 {
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px dashed var(--fl-border);
}

.p12-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}

.p12-head strong {
  font-size: 13px;
}

.p12-body {
  margin-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.p12-row {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.p12-row input {
  flex: 1;
  min-width: 200px;
  max-width: 320px;
}

.danger-text {
  margin: 0;
  padding: 8px 10px;
  border-radius: var(--fl-radius-sm);
  background: var(--fl-red-soft);
  color: var(--fl-red);
  font-size: 12px;
  line-height: 1.6;
}

/* 外层只负责撑满并裁剪，内层负责滚动：
   直接把 height:100% + overflow 放在同一个 flex 容器上，在部分 WebView 里
   会出现"内容溢出但滚不动"的行为，因此拆成两层，与流量页保持同一套结构。 */
.cert {
  /* 该视图现在只挂在设置抽屉的 .drawer-body（常规文档流、由父级滚动）里，
     不再作为独立页面用 flex 撑满；这里只保留内边距与列布局。 */
  display: flex;
  flex-direction: column;
}

.cert-body {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.card {
  background: var(--fl-panel);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius);
  padding: 16px 18px;
}

.intro {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.intro-text h3 {
  margin: 0 0 2px;
  font-size: 15px;
}

.intro-text p {
  margin: 0;
  font-size: 12px;
}

.progress {
  display: flex;
  gap: 6px;
  list-style: none;
  margin: 0;
  padding: 0;
}

.progress li {
  font-size: 11px;
  padding: 3px 9px;
  border-radius: 999px;
  border: 1px solid var(--fl-border);
  color: var(--fl-muted);
  background: var(--fl-panel-2);
  white-space: nowrap;
}

.progress li.ok {
  color: var(--fl-green);
  background: var(--fl-green-soft);
  border-color: transparent;
}

.step.ok {
  border-color: color-mix(in srgb, var(--fl-green) 30%, var(--fl-border));
}

.step-head {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  margin-bottom: 12px;
}

.step-head h3 {
  margin: 0 0 2px;
  font-size: 14px;
}

.step-head p {
  margin: 0;
  font-size: 12px;
}

.grow {
  flex: 1;
  min-width: 0;
}

.no {
  flex: none;
  width: 22px;
  height: 22px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  font-size: 12px;
  font-weight: 600;
  color: var(--fl-text-2);
  background: var(--fl-panel-2);
  border: 1px solid var(--fl-border-strong);
}

.step.ok .no {
  color: #fff;
  background: var(--fl-green);
  border-color: transparent;
}

.badge {
  flex: none;
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 999px;
  border: 1px solid transparent;
  white-space: nowrap;
}

.badge.ok {
  color: var(--fl-green);
  background: var(--fl-green-soft);
}

.badge.warn {
  color: var(--fl-orange);
  background: var(--fl-orange-soft);
}

.badge.todo {
  color: var(--fl-muted);
  background: var(--fl-panel-2);
  border-color: var(--fl-border);
}

.status-line {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0 0 10px;
  font-size: 12px;
  color: var(--fl-text-2);
}

.dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  flex: none;
}

.dot.ok {
  background: var(--fl-green);
}

.dot.warn {
  background: var(--fl-orange);
}

.kv {
  display: grid;
  grid-template-columns: 84px 1fr;
  gap: 8px 14px;
  margin: 10px 0;
  font-size: 12px;
}

.kv dt {
  color: var(--fl-muted);
}

.kv dd {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.mono {
  font-family: var(--fl-mono);
  font-size: 11px;
}

.small {
  font-size: 11px;
}

.break {
  word-break: break-all;
}

.muted {
  color: var(--fl-muted);
}

.warn-text {
  color: var(--fl-orange);
}

.row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.addr {
  font-family: var(--fl-mono);
  font-size: 12px;
  padding: 4px 8px;
  background: var(--fl-panel-2);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  word-break: break-all;
}

.actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 10px;
}

.ghost {
  border-color: var(--fl-border);
  background: transparent;
  color: var(--fl-text-2);
  font-size: 12px;
}

.ghost.tiny {
  padding: 1px 6px;
  font-size: 11px;
}

.danger {
  color: var(--fl-red);
  border-color: color-mix(in srgb, var(--fl-red) 35%, var(--fl-border));
  background: transparent;
}

.danger:hover {
  background: var(--fl-red-soft);
}

.link {
  border: none;
  background: none;
  padding: 0;
  color: var(--fl-accent);
  font-size: inherit;
  text-decoration: underline;
}

.disclose {
  border: none;
  background: none;
  padding: 0;
  margin-top: 10px;
  color: var(--fl-text-2);
  font-size: 12px;
}

.cmd {
  margin: 8px 0 0;
  padding: 10px;
  background: var(--fl-panel-2);
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius-sm);
  font-family: var(--fl-mono);
  font-size: 11px;
  line-height: 1.6;
  max-height: 200px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}

.phone {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  flex-wrap: wrap;
}

.qr-box {
  flex: none;
  padding: 8px;
  background: #fff;
  border: 1px solid var(--fl-border);
  border-radius: var(--fl-radius);
  line-height: 0;
}

.qr {
  width: 224px;
  height: 224px;
  display: block;
}

.empty-box {
  padding: 12px 14px;
  border: 1px dashed var(--fl-border-strong);
  border-radius: var(--fl-radius-sm);
  font-size: 12px;
}

.empty-box p {
  margin: 0 0 8px;
}

.tip {
  margin: 10px 0 0;
  font-size: 12px;
}

.warn-box {
  padding: 10px 12px;
  border-radius: var(--fl-radius-sm);
  background: var(--fl-orange-soft);
  color: var(--fl-orange);
  border: 1px solid color-mix(in srgb, var(--fl-orange) 30%, transparent);
}

.result {
  margin-top: 10px;
  padding: 10px 12px;
  border-radius: var(--fl-radius-sm);
  border: 1px solid var(--fl-border);
  background: var(--fl-panel-2);
  font-size: 12px;
}

.result.ok {
  border-color: color-mix(in srgb, var(--fl-green) 35%, transparent);
  background: var(--fl-green-soft);
}

.result.bad {
  border-color: color-mix(in srgb, var(--fl-orange) 35%, transparent);
  background: var(--fl-orange-soft);
}

.result p {
  margin: 0 0 4px;
}

.result-title {
  font-weight: 600;
}

.result-msg {
  color: var(--fl-text);
  word-break: break-word;
}
</style>
