package certsrv

import (
	"html/template"
	"io"
)

// pageModel 是下载页的渲染模型。
type pageModel struct {
	CommonName    string // 证书 CN，例如 FlowLens Root CA
	Fingerprint   string // 完整 SHA-256 指纹
	ShortFinger   string // 指纹前 4 组，页面上方展示
	ShortFingerID string // 由证书派生的 UUID，仅用于展示一致性
	NotAfter      string // 到期日
	BaseURL       string // 当前访问地址，供用户手抄到其他设备
	Version       string // 应用版本
}

// pageTmpl 是证书下载页。刻意保持单文件、内联样式、无外部依赖：
// 手机上哪怕只有 2G 网络也能一次加载完，不会因为等 CSS 而卡住安装流程。
var pageTmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
<title>安装 观流 证书</title>
<style>
  :root {
    --bg: #f6f7f9; --card: #fff; --text: #16181d; --text2: #5b6270;
    --line: #e5e7ec; --accent: #2f6feb; --accent2: #eaf1ff;
    --warn-bg: #fff6e5; --warn-line: #f0c274; --warn-text: #7a4b00;
    --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #16181d; --card: #1d2027; --text: #e8eaf0; --text2: #9aa3b5;
      --line: #2c313c; --accent: #6c9dff; --accent2: #1f2a44;
      --warn-bg: #2e2413; --warn-line: #6b5220; --warn-text: #f0c274;
    }
  }
  * { box-sizing: border-box; -webkit-tap-highlight-color: transparent; }
  body {
    margin: 0; padding: 20px 16px 40px; background: var(--bg); color: var(--text);
    font: 15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
          "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  }
  .wrap { max-width: 520px; margin: 0 auto; }
  h1 { font-size: 20px; margin: 0 0 4px; letter-spacing: -0.01em; }
  .lead { margin: 0 0 18px; color: var(--text2); font-size: 13px; }
  .card {
    background: var(--card); border: 1px solid var(--line); border-radius: 14px;
    padding: 16px; margin-bottom: 14px;
  }
  .cert-name { font-weight: 600; margin-bottom: 8px; word-break: break-all; }
  .kv { display: grid; grid-template-columns: 66px 1fr; gap: 6px 10px; font-size: 13px; }
  .kv dt { color: var(--text2); }
  .kv dd { margin: 0; word-break: break-all; }
  .mono { font-family: var(--mono); font-size: 12px; }
  .btn {
    display: flex; align-items: center; justify-content: space-between; gap: 8px;
    width: 100%; padding: 14px 16px; margin-top: 10px; border-radius: 12px;
    border: 1px solid var(--line); background: var(--card); color: var(--text);
    font-size: 15px; font-weight: 500; text-decoration: none;
  }
  .btn.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
  .btn .arrow { color: var(--text2); font-size: 13px; }
  .btn.primary .arrow { color: rgba(255, 255, 255, 0.8); }
  .btn small { display: block; font-weight: 400; font-size: 12px; opacity: 0.75; margin-top: 2px; }
  .warn {
    background: var(--warn-bg); border: 1px solid var(--warn-line); color: var(--warn-text);
    border-radius: 12px; padding: 12px 14px; font-size: 13px; margin-bottom: 14px;
  }
  .alert {
    background: var(--card); border: 1px solid var(--accent); border-radius: 14px;
    padding: 14px 16px; margin-bottom: 14px;
  }
  .alert h2 { font-size: 15px; margin: 0 0 6px; color: var(--accent); }
  .alert p { font-size: 13px; color: var(--text2); margin: 6px 0; }
  .alert strong { color: var(--text); }
  .check {
    display: flex; align-items: center; justify-content: space-between; gap: 8px;
    margin-top: 12px; padding: 12px 14px; border-radius: 10px;
    background: var(--accent2); color: var(--accent); font-size: 13px;
    text-decoration: none; font-weight: 600;
  }
  details { border-top: 1px solid var(--line); padding: 12px 0 0; margin-top: 12px; }
  details:first-of-type { border-top: none; margin-top: 0; }
  summary { cursor: pointer; font-weight: 600; font-size: 14px; }
  ol { margin: 10px 0 0; padding-left: 20px; color: var(--text2); font-size: 13px; }
  ol li { margin: 6px 0; }
  code { font-family: var(--mono); font-size: 12px; background: var(--accent2); padding: 1px 5px; border-radius: 5px; }
  .foot { text-align: center; color: var(--text2); font-size: 12px; margin-top: 22px; }
</style>
</head>
<body>
<div class="wrap">
  <h1>安装 观流 证书</h1>
  <p class="lead">装好这张根证书，这台设备上的 HTTPS 流量才能被 观流 解密查看。</p>

  <div class="card">
    <div class="cert-name">{{.CommonName}}</div>
    <dl class="kv">
      <dt>指纹</dt><dd class="mono">{{.ShortFinger}}</dd>
      <dt>有效期至</dt><dd>{{.NotAfter}}</dd>
      <dt>下载地址</dt><dd class="mono">{{.BaseURL}}</dd>
    </dl>
  </div>

  <div class="warn">
    只在你自己的设备上安装。这是 观流 在你电脑上生成的自签证书，
    安装等于允许持有对应私钥的一方解密本机 HTTPS 流量。
  </div>

  <div class="alert">
    <h2>Safari 提示「无法验证服务器身份」？</h2>
    <p><strong>装完描述文件还不算信任。</strong>iPhone 从 iOS 10.3 起，用户装的根证书默认关闭信任，必须再手动打开一次：</p>
    <p>设置 → 通用 → 关于本机 → 最底部「证书信任设置」→ 把 <code>{{.CommonName}}</code> 的开关打开。</p>
    <p>如果那里找不到这张证书，说明描述文件还没装（回到下面下载）；如果列表里有多张同名证书，以指纹 <code>{{.ShortFinger}}</code> 为准，删掉旧的再装。</p>
    <a class="check" href="https://example.com/" target="_blank" rel="noopener">点此验证信任是否生效 <span>›</span></a>
    <p style="margin-top:6px">开着代理点上面链接：能正常打开 example.com 就说明信任已生效；仍提示无法验证，就重复上面的开关步骤。</p>
  </div>

  <div class="card">
    <a class="btn primary" href="/ca.mobileconfig">
      <span>iPhone / iPad / Mac<small>下载描述文件，一步安装</small></span>
      <span class="arrow">›</span>
    </a>
    <a class="btn" href="/ca.crt">
      <span>Android / Windows / 其他<small>下载证书文件 .crt</small></span>
      <span class="arrow">›</span>
    </a>

    <details open id="ios">
      <summary>iPhone / iPad 安装步骤</summary>
      <ol>
        <li>点上面的「下载描述文件」，Safari 会提示「已下载描述文件」</li>
        <li>打开「设置」→ 顶部「已下载描述文件」→ 安装</li>
        <li>再进「设置」→「通用」→「关于本机」→ 底部「证书信任设置」</li>
        <li><strong>打开 <code>{{.CommonName}}</code> 的开关</strong>——不做这一步 Safari 会提示「无法验证服务器身份」，仍然抓不到 HTTPS</li>
      </ol>
    </details>

    <details id="android">
      <summary>Android 安装步骤</summary>
      <ol>
        <li>点「下载证书文件」，保存后在通知栏点开它</li>
        <li>系统会要求设置锁屏密码（已有则跳过）</li>
        <li>「设置」→「安全」→「加密与凭据」→「安装证书」→「CA 证书」→ 选择刚下载的文件</li>
        <li>注意：Android 7 起，多数 App 不信任用户安装的 CA，只能抓浏览器等部分流量</li>
      </ol>
    </details>

    <details id="windows">
      <summary>Windows 安装步骤</summary>
      <ol>
        <li>下载 <code>.crt</code> 后双击打开</li>
        <li>「安装证书」→「当前用户」→「将所有的证书都放入下列存储」</li>
        <li>浏览 → 「受信任的根证书颁发机构」→ 完成</li>
      </ol>
    </details>
  </div>

  <div class="foot">观流 {{.Version}} · 证书 ID {{.ShortFingerID}}</div>
</div>
<script>
  // 按 UA 只展开当前设备对应的步骤：三份说明同时摊开会让用户不知道看哪段。
  (function () {
    var ua = navigator.userAgent || '';
    var id = /iPhone|iPad|iPod|Macintosh/.test(ua) ? 'ios'
           : /Android/.test(ua) ? 'android'
           : /Windows/.test(ua) ? 'windows' : '';
    if (!id) return;
    var all = document.querySelectorAll('details');
    for (var i = 0; i < all.length; i++) all[i].open = all[i].id === id;
  })();
</script>
</body>
</html>
`))

// renderPage 把模型渲染到响应流。
func renderPage(w io.Writer, m pageModel) error {
	return pageTmpl.Execute(w, m)
}
