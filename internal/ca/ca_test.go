package ca_test

import (
	"crypto"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/kerison/flowlens/internal/ca"
)

func TestManagerGeneratesAndPersistsRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	m1, err := ca.NewManager(dir)
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}

	root := m1.GetRootCA()
	if root == nil || !root.IsCA {
		t.Fatal("根证书无效或不是 CA")
	}
	if root.Subject.CommonName != "FlowLens Root CA" {
		t.Fatalf("根证书主体不正确: %s", root.Subject.CommonName)
	}
	if !root.NotAfter.After(time.Now().AddDate(9, 0, 0)) {
		t.Fatalf("根证书有效期异常: %s", root.NotAfter)
	}

	// 私钥必须是 0600，否则同机其它用户可伪造证书
	info, err := os.Stat(filepath.Join(dir, "flowlens-ca.key"))
	if err != nil {
		t.Fatalf("私钥文件不存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("私钥权限应为 0600，实际 %o", perm)
	}

	// 重新加载应复用同一张证书，而不是重新生成
	m2, err := ca.NewManager(dir)
	if err != nil {
		t.Fatalf("重新加载 CA 失败: %v", err)
	}
	if !m2.GetRootCA().Equal(root) {
		t.Fatal("重新加载后根证书发生变化，持久化失败")
	}
}

func TestLeafCertSignedByRoot(t *testing.T) {
	m, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}

	leaf, err := m.GetCert("api.example.com")
	if err != nil {
		t.Fatalf("签发叶子证书失败: %v", err)
	}
	if len(leaf.Certificate) != 2 {
		t.Fatalf("叶子证书应包含链（叶子+根），实际 %d 张", len(leaf.Certificate))
	}

	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析叶子证书失败: %v", err)
	}
	if parsed.Subject.CommonName != "api.example.com" {
		t.Fatalf("CN 不正确: %s", parsed.Subject.CommonName)
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "api.example.com" {
		t.Fatalf("SAN 不正确: %v", parsed.DNSNames)
	}
	if !parsed.NotAfter.Before(time.Now().AddDate(0, 0, 400)) {
		t.Fatalf("叶子证书有效期过长，可能触发平台限制: %s", parsed.NotAfter)
	}

	// 用根证书验证签名，确保链可被客户端接受
	roots := x509.NewCertPool()
	roots.AddCert(m.GetRootCA())
	if _, err := parsed.Verify(x509.VerifyOptions{
		Roots:     roots,
		DNSName:   "api.example.com",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("叶子证书验签失败: %v", err)
	}
}

func TestLeafCertForIP(t *testing.T) {
	m, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	// 带端口的 IP 也应能正确剥离并写入 IP SAN
	leaf, err := m.GetCert("192.168.1.10:443")
	if err != nil {
		t.Fatalf("为 IP 签发证书失败: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if len(parsed.IPAddresses) != 1 || parsed.IPAddresses[0].String() != "192.168.1.10" {
		t.Fatalf("IP SAN 不正确: %v", parsed.IPAddresses)
	}
}

func TestLeafCertCached(t *testing.T) {
	m, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	a, _ := m.GetCert("cache.example.com")
	b, _ := m.GetCert("cache.example.com")
	if a != b {
		t.Fatal("同一域名应命中缓存，返回同一证书指针")
	}
}

// 导出 .p12 是本项目唯一交出私钥的路径，必须验证：
// 打包出来的东西能被标准库解回同一把钥匙，且密码确实生效。
func TestExportPKCS12RoundTrip(t *testing.T) {
	m, err := ca.NewManager(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}

	const password = "flowlens-test-1"
	data, err := m.ExportPKCS12(password)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("导出内容为空")
	}

	key, cert, err := pkcs12.Decode(data, password)
	if err != nil {
		t.Fatalf("用同一密码解码失败: %v", err)
	}
	if cert == nil || !cert.IsCA {
		t.Fatal("解出的证书不是 CA")
	}
	if cert.Subject.CommonName != "FlowLens Root CA" {
		t.Fatalf("证书主体不一致: %s", cert.Subject.CommonName)
	}
	// 私钥必须与根证书配对，否则导出的 p12 在客户端侧会静默不可用
	signer, ok := key.(crypto.Signer)
	if !ok {
		t.Fatalf("解出的私钥类型不支持签名: %T", key)
	}
	if !cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool }).Equal(signer.Public()) {
		t.Fatal("p12 里的私钥与证书不匹配")
	}

	// 错误密码必须解不开：解开了说明密码没真正参与加密
	if _, _, err := pkcs12.Decode(data, "wrong-password"); err == nil {
		t.Fatal("错误密码不应解码成功")
	}
}

func TestExportPKCS12RejectsShortPassword(t *testing.T) {
	m, err := ca.NewManager(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	if _, err := m.ExportPKCS12(""); err == nil {
		t.Fatal("空密码应被拒绝")
	}
	if _, err := m.ExportPKCS12("1234567"); err == nil {
		t.Fatal("少于 8 位应被拒绝")
	}
}
