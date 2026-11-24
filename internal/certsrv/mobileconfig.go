package certsrv

import (
	"bytes"
	"fmt"
	"text/template"
)

// mobileconfigTmpl 是 iOS / macOS 的配置描述文件（Configuration Profile）模板。
//
// 关键点：
//   - PayloadType 必须是 com.apple.security.root，iOS 才知道这是要装进信任库的根证书；
//   - PayloadContent 是 DER 的 base64，不能是 PEM 文本；
//   - 顶层 PayloadType 固定为 Configuration，否则「设置」里不会出现安装入口；
//   - UUID 由证书内容派生（见 profileUUID），重复下载等于覆盖同一个描述文件。
var mobileconfigTmpl = template.Must(template.New("mobileconfig").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
		<dict>
			<key>PayloadCertificateFileName</key>
			<string>flowlens-ca.crt</string>
			<key>PayloadContent</key>
			<data>{{.CertBase64}}</data>
			<key>PayloadDescription</key>
			<string>由 观流 生成的根证书，用于解密本机 HTTPS 调试流量。</string>
			<key>PayloadDisplayName</key>
			<string>{{.CommonName}}</string>
			<key>PayloadIdentifier</key>
			<string>com.flowlens.ca.{{.UUID}}</string>
			<key>PayloadType</key>
			<string>com.apple.security.root</string>
			<key>PayloadUUID</key>
			<string>{{.UUID}}</string>
			<key>PayloadVersion</key>
			<integer>1</integer>
		</dict>
	</array>
	<key>PayloadDescription</key>
	<string>安装后请到「设置 → 通用 → 关于本机 → 证书信任设置」中开启完全信任。</string>
	<key>PayloadDisplayName</key>
	<string>观流 根证书</string>
	<key>PayloadIdentifier</key>
	<string>com.flowlens.profile.{{.UUID}}</string>
	<key>PayloadOrganization</key>
	<string>观流</string>
	<key>PayloadRemovalDisallowed</key>
	<false/>
	<key>PayloadType</key>
	<string>Configuration</string>
	<key>PayloadUUID</key>
	<string>{{.ProfileUUID}}</string>
	<key>PayloadVersion</key>
	<integer>1</integer>
</dict>
</plist>
`))

// mobileconfigModel 是描述文件的渲染模型。
type mobileconfigModel struct {
	CommonName  string
	UUID        string // 证书载荷 UUID（由证书派生）
	ProfileUUID string // 描述文件本身 UUID（同样派生，保证稳定）
	CertBase64  string
}

// buildMobileconfig 生成描述文件字节。
func buildMobileconfig(opts Options) ([]byte, error) {
	if len(opts.CertDER) == 0 {
		return nil, fmt.Errorf("缺少证书内容")
	}
	id := profileUUID(opts.CertDER)
	m := mobileconfigModel{
		CommonName:  opts.CommonName,
		UUID:        id,
		ProfileUUID: profileUUID(append([]byte("profile"), opts.CertDER...)),
		CertBase64:  base64DER(opts.CertDER),
	}
	var buf bytes.Buffer
	if err := mobileconfigTmpl.Execute(&buf, m); err != nil {
		return nil, fmt.Errorf("渲染描述文件失败: %w", err)
	}
	return buf.Bytes(), nil
}
