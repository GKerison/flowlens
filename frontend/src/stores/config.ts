/**
 * 配置与连接域：代理状态、应用配置、根证书、信任、证书分发、系统代理、自检。
 *
 * 这里的对象生命周期跟"后端进程"一致（Bootstrap 一次、事件增量更新），
 * 与会话数据（traffic 域）和界面偏好（ui 域）互不掺杂。
 */
import { defineStore } from 'pinia'
import { Dialogs } from '@wailsio/runtime'
import { useI18n } from '../i18n'
import { App as Backend } from '../../bindings/github.com/kerison/flowlens/internal/service'
import type {
  AppConfig,
  Bootstrap,
  CAExportResult,
  CAInfo,
  CertServerInfo,
  Locale,
  ProxyState,
  SelfTestResult,
  SysProxyResult,
  SysProxyStatus,
  TrustResult,
  TrustStatus,
  UIStyle,
} from '../types'
import { toMessage } from './helpers'
import { useUiStore } from './ui'

export const useConfigStore = defineStore('config', {
  state: () => ({
    bootstrap: null as Bootstrap | null,
    proxy: { running: false, addr: '', paused: false, flows: 0, startedAt: '' } as ProxyState,
    ca: null as CAInfo | null,
    config: null as AppConfig | null,
    /** 根证书在本机的信任状态（可刷新） */
    trust: null as TrustStatus | null,
    /** 证书分发服务状态与地址 */
    cert: null as CertServerInfo | null,
    /** 系统代理接管状态 */
    sysProxy: null as SysProxyStatus | null,
    /** 启动自愈的结论，显示一条横幅 */
    sysProxyNote: '',
    sysProxyBusy: false,
    /** 信任安装/卸载进行中，用于禁用按钮并避免重复点击 */
    trustBusy: false,
    /** 最近一次自检结果 */
    selfTest: null as SelfTestResult | null,
    selfTestBusy: false,
  }),

  getters: {
    /**
     * 给任意代理地址补上认证凭据。顶栏下拉里的手机地址与本机地址都要用，
     * 避免"复制手机地址却少了密码"这种最难排查的连不上问题。
     */
    withCredentials(state): (base: string) => string {
      return (base: string): string => {
        const cfg = state.config
        if (!base || !cfg?.requireAuth || !cfg.authPassword) return base
        const cred = `${encodeURIComponent(cfg.authUsername)}:${encodeURIComponent(cfg.authPassword)}`
        return base.replace('://', `://${cred}@`)
      }
    },
  },

  actions: {
    /** 配置兜底：保证数组字段不为 null，避免表单渲染时报错。 */
    normalizeConfig(raw: AppConfig): AppConfig {
      return {
        ...raw,
        skipDecryptHosts: raw.skipDecryptHosts ?? [],
        mapLocalRules: raw.mapLocalRules ?? [],
        mapRemoteRules: raw.mapRemoteRules ?? [],
      }
    },

    /** 用 Bootstrap 结果填充首屏状态（init 时调用一次）。 */
    applyBootstrap(bs: Bootstrap): void {
      const ui = useUiStore()
      this.bootstrap = bs
      this.proxy = bs.state
      this.ca = bs.ca
      this.config = this.normalizeConfig(bs.config)
      this.trust = bs.trust ?? null
      this.cert = bs.cert ?? null
      this.sysProxy = bs.sysProxy ?? null
      this.sysProxyNote = bs.sysProxyNote || ''
      this.applyPreferences(this.config)
    },

    /* ---------------- 代理 ---------------- */

    /** 把配置里的界面偏好（主题、语言、风格）同步到 ui store。 */
    applyPreferences(cfg: AppConfig): void {
      const ui = useUiStore()
      ui.theme = (cfg.theme as typeof ui.theme) || 'system'
      ui.applyTheme()
      ui.setLocale((cfg.locale as Locale) || 'zh-CN')
      ui.setStyle((cfg.uiStyle as UIStyle) || 'standard')
    },

    /** 启动/停止代理。 */
    async toggleProxy(): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      try {
        this.proxy = this.proxy.running ? await Backend.StopProxy() : await Backend.StartProxy()
        ui.showToast(this.proxy.running ? t('代理已启动') : t('代理已停止'), 'success')
      } catch (err) {
        // 停止失败通常是因为系统代理摘不掉（需要授权），必须让用户看见原因
        ui.showToast(toMessage(err), 'error')
      } finally {
        // 停止代理会顺带恢复系统代理，状态必须跟着刷新
        await this.refreshSysProxy()
      }
    },

    /** 暂停/恢复抓取。暂停时代理退化为纯隧道中转，上网不受影响。 */
    async togglePause(): Promise<void> {
      try {
        const { t } = useI18n()
        const next = !this.proxy.paused
        this.proxy = await Backend.SetPaused(next)
        useUiStore().showToast(
          next
            ? t('已暂停：仅中转，上网不受影响')
            : t('已恢复抓包：HTTPS 重新解密'),
          'info',
        )
      } catch (err) {
        useUiStore().showToast(toMessage(err), 'error')
      }
    },

    /** 重启代理使配置生效。 */
    async restartProxy(): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      try {
        this.proxy = await Backend.RestartProxy()
        ui.showToast(t('代理已重启'), 'success')
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
      }
    },

    /* ---------------- 配置 ---------------- */

    /** 保存配置；若代理正在运行则提示重启生效。 */
    async saveConfig(next: AppConfig): Promise<boolean> {
      const ui = useUiStore()
      const { t } = useI18n()
      try {
        const saved = (await Backend.SaveConfig(next)) as AppConfig
        this.config = saved
        this.applyPreferences(saved)
        await this.refreshSysProxy()
        // 端口或监听地址可能变了，证书服务的地址随之变化；刷新一次省得用户看到旧二维码
        void this.refreshCert()
        ui.showToast(t('配置已保存'), 'success')
        return true
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
        return false
      }
    },

    /* ---------------- 配置导入导出（F9-3） ---------------- */

    /** 导出当前配置。target 为另存为对话框选定的路径，取消返回 null。 */
    async exportConfig(target: string | null): Promise<string | null> {
      const ui = useUiStore()
      const { t } = useI18n()
      try {
        const path = target ?? (await this.pickSavePath('flowlens-config.json'))
        if (!path) return null
        const res = await Backend.ExportConfig(path)
        ui.showToast(t('配置已导出到 {path}', { path: res.path }), 'success')
        return res.path
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
        return null
      }
    },

    /** 从文件导入配置并立即生效。 */
    async importConfig(): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      let path: string | null = null
      try {
        const picked = await Dialogs.OpenFile({
          Title: t('选择 观流 配置文件'),
          CanChooseFiles: true,
          CanChooseDirectories: false,
          AllowsMultipleSelection: false,
        })
        path = Array.isArray(picked) ? picked[0] : picked
      } catch {
        ui.showToast(t('当前环境无法打开文件选择框'), 'error')
        return
      }
      if (!path) return

      // 导入会整体覆盖当前配置（含端口与解密开关），让用户知情后再动手
      const ok = await ui.confirmAction({
        title: t('导入配置'),
        message: t('导入将覆盖当前全部配置（监听端口、解密开关、映射规则、断点设置等）。继续？'),
        confirmText: t('导入'),
      })
      if (!ok) return

      try {
        const saved = (await Backend.ImportConfig(path)) as AppConfig
        this.config = this.normalizeConfig(saved)
        this.applyPreferences(saved)
        await this.refreshSysProxy()
        void this.refreshCert()
        ui.showToast(t('配置已导入并生效'), 'success')
      } catch (err) {
        ui.showToast(t('导入失败：{message}', { message: toMessage(err) }), 'error')
      }
    },

    /** 原生保存对话框；失败时返回 null（用户取消或环境不支持）。 */
    async pickSavePath(defaultName: string): Promise<string | null> {
      const { t } = useI18n()
      try {
        const picked = await Dialogs.SaveFile({
          Title: t('保存配置'),
          Filename: defaultName,
          CanCreateDirectories: true,
        })
        return picked || null
      } catch {
        return null
      }
    },

    /* ---------------- 系统代理 ---------------- */

    /** 刷新系统代理接管状态。 */
    async refreshSysProxy(): Promise<void> {
      try {
        this.sysProxy = (await Backend.SysProxyStatus()) as unknown as SysProxyStatus
      } catch {
        // 状态查询失败不值得打扰用户：界面会退化成"未知"
      }
    },

    /** 接管系统代理。前置条件是代理已启动。 */
    async enableSysProxy(): Promise<void> {
      const ui = useUiStore()
      if (this.sysProxyBusy) return
      this.sysProxyBusy = true
      try {
        const res = (await Backend.EnableSysProxy()) as unknown as SysProxyResult
        this.sysProxy = res.status
        this.sysProxyNote = ''
        ui.showToast(res.message, 'success')
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
        await this.refreshSysProxy()
      } finally {
        this.sysProxyBusy = false
      }
    },

    /** 恢复系统代理原设置。 */
    async disableSysProxy(): Promise<boolean> {
      const ui = useUiStore()
      if (this.sysProxyBusy) return false
      this.sysProxyBusy = true
      try {
        const res = (await Backend.DisableSysProxy()) as unknown as SysProxyResult
        this.sysProxy = res.status
        this.sysProxyNote = ''
        ui.showToast(res.message, 'success')
        return true
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
        await this.refreshSysProxy()
        return false
      } finally {
        this.sysProxyBusy = false
      }
    },

    /** 顶部开关：已接管则恢复，未接管则接管。 */
    async toggleSysProxy(): Promise<void> {
      if (this.sysProxy?.enabled) {
        await this.disableSysProxy()
      } else {
        await this.enableSysProxy()
      }
    },

    /* ---------------- 证书与信任 ---------------- */

    /** 重新生成根证书。 */
    async regenerateCA(): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      const ok = await ui.confirmAction({
        title: t('重新生成根证书'),
        message: t('重新生成后，已安装的旧证书将失效，需要重新安装。确认继续？'),
        confirmText: t('重新生成'),
        danger: true,
      })
      if (!ok) return
      try {
        this.ca = await Backend.RegenerateCA()
        // 换了证书：信任状态必然失效，下载页也被后端重启过，两边都要同步
        await Promise.all([this.refreshTrust(), this.refreshCert()])
        this.selfTest = null
        ui.showToast(t('已生成新的根证书，需要重新安装到设备'), 'success')
      } catch (err) {
        ui.showToast(toMessage(err), 'error')
      }
    },

    /** 刷新本机信任状态。 */
    async refreshTrust(): Promise<void> {
      const { t } = useI18n()
      try {
        this.trust = (await Backend.TrustStatus()) as unknown as TrustStatus
      } catch (err) {
        useUiStore().showToast(
          t('读取信任状态失败：{message}', { message: toMessage(err) }),
          'error',
        )
      }
    },

    /**
     * 安装根证书到本机信任库。
     * elevated=false 优先用户级（无需密码）；失败时上层可引导用户改用系统级。
     */
    async installTrust(elevated = false): Promise<boolean> {
      const ui = useUiStore()
      const { t } = useI18n()
      if (this.trustBusy) return false
      this.trustBusy = true
      try {
        const res = (await Backend.InstallTrust(elevated)) as unknown as TrustResult
        await this.refreshTrust()
        ui.showToast(res.message || t('安装完成'), 'success')
        return true
      } catch (err) {
        // 安装失败不弹原生对话框：用户可能只是拒绝了授权，属于正常路径
        ui.showToast(t('安装失败：{message}', { message: toMessage(err) }), 'error')
        await this.refreshTrust()
        return false
      } finally {
        this.trustBusy = false
      }
    },

    /** 从本机信任库移除根证书。 */
    async uninstallTrust(): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      if (this.trustBusy) return
      const ok = await ui.confirmAction({
        title: t('卸载根证书'),
        message: t('卸载后本机将不再解密 HTTPS，浏览器会提示证书不受信任。确认卸载？'),
        confirmText: t('卸载'),
        danger: true,
      })
      if (!ok) return
      this.trustBusy = true
      try {
        const res = (await Backend.UninstallTrust()) as unknown as TrustResult
        await this.refreshTrust()
        this.selfTest = null
        ui.showToast(res.message || t('已卸载'), 'success')
      } catch (err) {
        ui.showToast(t('卸载失败：{message}', { message: toMessage(err) }), 'error')
        await this.refreshTrust()
      } finally {
        this.trustBusy = false
      }
    },

    /** 刷新证书分发服务状态。 */
    async refreshCert(): Promise<void> {
      const { t } = useI18n()
      try {
        this.cert = (await Backend.CertServer()) as unknown as CertServerInfo
      } catch (err) {
        useUiStore().showToast(
          t('读取证书服务状态失败：{message}', { message: toMessage(err) }),
          'error',
        )
      }
    },

    /** 按当前配置重启证书分发服务（改端口/监听地址后用）。 */
    async restartCertServer(): Promise<void> {
      const ui = useUiStore()
      const { t } = useI18n()
      try {
        this.cert = (await Backend.RestartCertServer()) as unknown as CertServerInfo
        ui.showToast(t('证书服务已重启'), 'success')
      } catch (err) {
        await this.refreshCert()
        ui.showToast(toMessage(err), 'error')
      }
    },

    /** 导出含私钥的 .p12。调用前必须由界面完成二次确认。 */
    async exportCA(password: string): Promise<CAExportResult> {
      return (await Backend.ExportCA(password)) as unknown as CAExportResult
    },

    /* ---------------- 自检 ---------------- */

    /** 端到端自检：验证「代理 → 解密 → 证书信任」是否全通。 */
    async runSelfTest(): Promise<void> {
      const { t } = useI18n()
      if (this.selfTestBusy) return
      this.selfTestBusy = true
      this.selfTest = null
      try {
        this.selfTest = (await Backend.SelfTest()) as unknown as SelfTestResult
      } catch (err) {
        this.selfTest = {
          ok: false,
          message: t('自检请求失败：{message}', { message: toMessage(err) }),
          targetUrl: '',
          status: 0,
          durationMs: 0,
          flows: 0,
        }
      } finally {
        this.selfTestBusy = false
      }
    },
  },
})
