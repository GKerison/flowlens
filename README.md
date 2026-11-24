<p align="center">
  <img src="docs/assets/guanliu-icon.svg" width="120" alt="观流图标" />
</p>

# 观流（FlowLens）

跨平台 HTTP/HTTPS 抓包工作台。桌面原生客户端：开代理 → 装证书 → 扫码连手机，三步看到明文流量。

[English](./README_EN.md) · [功能需求](./REQUIREMENTS.md) · [开发约定](./CONVENTIONS.md) · [手工验证清单](./docs/manual-test.md)

> 状态：M1–M15 已完成。开箱可用，支持中英双语、三种界面风格、三大桌面平台。

## 快速开始

1. **启动代理**：点顶栏的「启动代理」。默认监听 `0.0.0.0:8888`，手机与电脑同 Wi-Fi 即可直连；只想本机用时去「设置 → 监听地址」改回 `127.0.0.1`
2. **安装证书**：设置 → 证书 → 扫码打开下载页。**iPhone 装完描述文件后必须再去「设置 → 通用 → 关于本机 → 证书信任设置」开启完全信任**，否则会提示无法验证服务器身份
3. **连手机**：手机 Wi-Fi 的 HTTP 代理填顶栏显示的本机/局域网地址（点地址可复制，开启认证时自动带凭据）
4. **看明文**：手机浏览器访问任意 HTTPS 网站，会话出现在列表，点击可在右侧查看请求/响应详情

暂停抓包时代理退化为**纯隧道中转**：不解密、不抓包，上网完全不受影响，但 Map Local 等改包规则也暂停生效；再点一次恢复解密抓包，无需重启。

## 主要功能

- **代理与解密**：自签 CA、系统信任一键安装/卸载、证书分发页与二维码、代理认证（HTTP Basic）、按域名跳过解密
- **流量分析**：列表/域名分组、表头排序、URL 与响应体内容搜索、UTF-8/GBK 等字符集探测、WebSocket/SSE 帧查看、JSON/图片/表单预览
- **数据处理**：HAR/JSON 导出（可另存为）、请求重放（可编辑 method/URL/头/体）、Map Local 本地映射、Map Remote 远程映射、断点改写
- **移动端接入**：虚拟网卡地址过滤、可用代理地址下拉、接入自检、iOS 完全信任指引与验证入口
- **体验**：中英双语（默认中文）、标准/舒展（大屏）/极简三种风格、界面动效、配置导入导出

## 构建

需要 Go 1.26+ 与 Node.js 18+。

```bash
# 开发模式
wails3 task dev

# 构建当前平台
wails3 task build

# 打包当前平台
wails3 task package

# 服务端模式（无 GUI，HTTP 提供界面）
wails3 task build:server
bin/flowlens-server
```

更多平台的交叉编译参数见 `Taskfile.yml` 与 `build/<平台>/Taskfile.yml`。

## 发版与分支约定

- `main` 是唯一可发版分支：**任何功能改动先开 feature 分支开发 → 合并回 main → 打 tag 打包发版**
- 推送 `v*` tag 会触发 GitHub Actions，自动生成 macOS / Windows / Linux 三平台产物并创建 Release
- Release 下载地址：<https://github.com/GKerison/flowlens/releases/latest>
- 三平台均使用 GitHub 托管 Runner；项目站点通过 GitHub Pages 自动发布
- 详见 `CONVENTIONS.md` 的分支工作流章节

## 安全说明

- 根证书私钥以 0600 权限保存，绝不输出到界面或日志
- `.p12` 私钥导出需二次确认、密码 ≥ 8 位、文件权限 0600
- 仅在自己的设备上安装根证书；安装等于允许持有私钥的一方解密该设备 HTTPS 流量
- 代理默认对所有局域网开放（`0.0.0.0`），**局域网模式强烈建议开启代理认证**

## 许可证

[MIT](./LICENSE) · 第三方依赖的许可证随各自模块保留；本地化的 `third_party/go-mitmproxy` 保留原许可证与补丁标记。
