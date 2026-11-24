// Package ca 负责 观流 自建根证书（CA）的生成、持久化与动态签发叶子证书。
//
// 该实现满足 go-mitmproxy 的 cert.CA 接口，可直接注入内核使用：
//
//	proxy.Options{NewCaFunc: func() (cert.CA, error) { return caManager, nil }}
//
// 安全约定：
//   - 私钥文件权限 0600，目录 0700，绝不打印到日志、绝不返回给前端；
//   - 根证书有效期 10 年，叶子证书 365 天（部分平台对 TLS 证书有效期有上限，
//     保持一年以内可规避兼容性问题）。
package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

const (
	certFileName = "flowlens-ca.pem"
	keyFileName  = "flowlens-ca.key"

	rootCommonName = "FlowLens Root CA"
	rootOrg        = "FlowLens"

	rootValidYears = 10
	leafValidDays  = 365
	// 叶子证书缓存上限，防止长时间运行后无限增长
	maxLeafCache = 1024

	// 导出 .p12 时的最简密码长度
	minExportPasswordLen = 8
)

// Manager 持有根证书与私钥，并按域名签发叶子证书。
// 并发安全：可同时被内核的多个连接 goroutine 调用。
type Manager struct {
	dir      string
	certPath string
	keyPath  string

	mu   sync.RWMutex
	cert *x509.Certificate
	key  crypto.Signer

	leafMu sync.Mutex
	leaves map[string]*tls.Certificate
}

// Info 是给界面展示的证书摘要，不含任何私钥信息。
type Info struct {
	Subject      string    `json:"subject"`
	CommonName   string    `json:"commonName"`
	Organization string    `json:"organization"`
	NotBefore    time.Time `json:"notBefore"`
	NotAfter     time.Time `json:"notAfter"`
	Fingerprint  string    `json:"fingerprint"` // SHA-256，大写冒号分隔
	PEM          string    `json:"pem"`         // 证书内容，供下载与安装
	Path         string    `json:"path"`        // PEM 文件路径
}

// NewManager 加载已有 CA；不存在或已失效时自动生成。
func NewManager(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建证书目录失败: %w", err)
	}
	m := &Manager{
		dir:      dir,
		certPath: filepath.Join(dir, certFileName),
		keyPath:  filepath.Join(dir, keyFileName),
		leaves:   make(map[string]*tls.Certificate),
	}
	if err := m.loadOrCreate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Dir 返回证书所在目录。
func (m *Manager) Dir() string { return m.dir }

// CertPath 返回根证书 PEM 文件路径。
func (m *Manager) CertPath() string { return m.certPath }

// GetRootCA 实现 cert.CA 接口，返回根证书。
func (m *Manager) GetRootCA() *x509.Certificate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert
}

// GetCert 实现 cert.CA 接口，按主机名签发（或取缓存）叶子证书。
// host 可能带端口（如 example.com:443），内部会剥离。
func (m *Manager) GetCert(host string) (*tls.Certificate, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, errors.New("签发证书失败: 主机名为空")
	}

	m.leafMu.Lock()
	if c, ok := m.leaves[host]; ok {
		m.leafMu.Unlock()
		return c, nil
	}
	m.leafMu.Unlock()

	cert, err := m.issueLeaf(host)
	if err != nil {
		return nil, err
	}

	m.leafMu.Lock()
	// 缓存超限时全量清空：叶子证书可随时重签，丢弃代价仅是一次密钥生成
	if len(m.leaves) >= maxLeafCache {
		m.leaves = make(map[string]*tls.Certificate, maxLeafCache)
	}
	m.leaves[host] = cert
	m.leafMu.Unlock()
	return cert, nil
}

// Info 返回根证书摘要信息。
func (m *Manager) Info() Info {
	m.mu.RLock()
	c := m.cert
	m.mu.RUnlock()

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	sum := sha256.Sum256(c.Raw)

	cn := c.Subject.CommonName
	org := ""
	if len(c.Subject.Organization) > 0 {
		org = c.Subject.Organization[0]
	}
	return Info{
		Subject:      c.Subject.String(),
		CommonName:   cn,
		Organization: org,
		NotBefore:    c.NotBefore,
		NotAfter:     c.NotAfter,
		Fingerprint:  formatFingerprint(sum[:]),
		PEM:          string(pemBytes),
		Path:         m.certPath,
	}
}

// ExportPKCS12 把根证书与私钥打包成 PKCS#12（.p12 / .pfx）。
//
// 这是**唯一**会把私钥交出去的能力，因此：
//   - 必须有密码，且长度不低于 8 位（空密码 p12 在 macOS 上会被钥匙串直接拒绝）；
//   - 返回的是内存字节，落盘与权限由调用方负责（service 层以 0600 写入用户指定位置）；
//   - 不做任何"顺手复制到剪贴板/上传"的便利功能，避免私钥无声扩散。
//
// 用途：Java keystore、Android 系统级证书、部分企业代理客户端只接受 p12。
func (m *Manager) ExportPKCS12(password string) ([]byte, error) {
	if len(password) < minExportPasswordLen {
		return nil, fmt.Errorf("导出密码至少需要 %d 位", minExportPasswordLen)
	}

	m.mu.RLock()
	cert, key := m.cert, m.key
	m.mu.RUnlock()
	if cert == nil || key == nil {
		return nil, errors.New("根证书未就绪")
	}

	// Modern 使用 AES-256 + PBKDF2，兼容 Windows/macOS/Linux 与 Java 8+；
	// Legacy 会被部分老客户端需要，但不该作为默认。
	data, err := pkcs12.Modern.Encode(key, cert, nil, password)
	if err != nil {
		return nil, fmt.Errorf("打包 PKCS#12 失败: %w", err)
	}
	return data, nil
}

// Regenerate 重新生成根证书。调用方必须先卸载旧证书，否则客户端会因证书不匹配报错。
// 返回新的证书摘要。
func (m *Manager) Regenerate() (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cert, key, err := generateRoot()
	if err != nil {
		return Info{}, err
	}
	if err := m.persist(cert, key); err != nil {
		return Info{}, err
	}
	m.cert = cert
	m.key = key

	m.leafMu.Lock()
	m.leaves = make(map[string]*tls.Certificate)
	m.leafMu.Unlock()

	return m.Info(), nil
}

// loadOrCreate 尝试加载磁盘上的 CA；失败（不存在、损坏、过期、私钥不匹配）则重建。
func (m *Manager) loadOrCreate() error {
	cert, key, err := m.load()
	if err == nil {
		m.cert = cert
		m.key = key
		return nil
	}

	// 文件存在但不可用：保留现场便于排查，然后重建
	if !errors.Is(err, os.ErrNotExist) {
		_ = os.Rename(m.certPath, m.certPath+".broken")
		_ = os.Rename(m.keyPath, m.keyPath+".broken")
	}

	cert, key, genErr := generateRoot()
	if genErr != nil {
		return fmt.Errorf("生成根证书失败: %w", genErr)
	}
	if persistErr := m.persist(cert, key); persistErr != nil {
		return persistErr
	}
	m.cert = cert
	m.key = key
	return nil
}

func (m *Manager) load() (*x509.Certificate, crypto.Signer, error) {
	certPEM, err := os.ReadFile(m.certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(m.keyPath)
	if err != nil {
		return nil, nil, err
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, errors.New("根证书 PEM 解析失败")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("根证书解析失败: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("私钥 PEM 解析失败")
	}
	var signer crypto.Signer
	switch keyBlock.Type {
	case "RSA PRIVATE KEY":
		signer, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	case "EC PRIVATE KEY":
		signer, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	case "PRIVATE KEY":
		var k any
		k, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err == nil {
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, nil, errors.New("私钥类型不支持签名")
			}
			signer = s
		}
	default:
		return nil, nil, fmt.Errorf("未知私钥格式: %s", keyBlock.Type)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("私钥解析失败: %w", err)
	}

	if !cert.IsCA {
		return nil, nil, errors.New("已存在的证书不是 CA")
	}
	if time.Now().After(cert.NotAfter) {
		return nil, nil, errors.New("根证书已过期")
	}
	// 校验公私钥匹配，避免文件被替换后签发出一堆无效证书
	if !publicKeyEqual(cert.PublicKey, signer.Public()) {
		return nil, nil, errors.New("证书与私钥不匹配")
	}
	return cert, signer, nil
}

// persist 将证书与私钥写入磁盘，权限收紧到 0600。
func (m *Manager) persist(cert *x509.Certificate, key crypto.Signer) error {
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})

	var keyPEM []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return fmt.Errorf("序列化私钥失败: %w", err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	default:
		return errors.New("不支持的私钥类型")
	}

	if err := os.WriteFile(m.certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("写入根证书失败: %w", err)
	}
	if err := os.WriteFile(m.keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("写入私钥失败: %w", err)
	}
	// 确保权限生效（文件已存在时 WriteFile 不会改权限）
	if err := os.Chmod(m.keyPath, 0o600); err != nil {
		return fmt.Errorf("设置私钥权限失败: %w", err)
	}
	return nil
}

// generateRoot 生成 RSA 2048 根证书。
// 用 RSA 而非 ECDSA：根证书要装进各种系统的信任库，RSA 兼容面最广。
func generateRoot() (*x509.Certificate, crypto.Signer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   rootCommonName,
			Organization: []string{rootOrg},
		},
		NotBefore:             now.Add(-24 * time.Hour), // 留出时差，避免客户端认为"尚未生效"
		NotAfter:              now.AddDate(rootValidYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true, // 只允许直接签发终端证书，不签发中间 CA
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// issueLeaf 为指定主机签发叶子证书。
// 叶子使用 ECDSA P-256：生成耗时约 0.1ms，相比 RSA 2048 的几十毫秒，
// 在首次访问大量域名时体验差异明显；现代客户端均支持。
func (m *Manager) issueLeaf(host string) (*tls.Certificate, error) {
	m.mu.RLock()
	root := m.cert
	rootKey := m.key
	m.mu.RUnlock()
	if root == nil || rootKey == nil {
		return nil, errors.New("根证书未就绪")
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成叶子密钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host, Organization: []string{rootOrg}},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(0, 0, leafValidDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("签发 %s 证书失败: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, root.Raw}, // 附带根证书，便于客户端构建链
		PrivateKey:  leafKey,
		Leaf:        nil,
	}, nil
}

// normalizeHost 去掉端口、方括号与末尾点，统一小写。
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// randomSerial 生成 128 位正随机序列号（CA/Browser Forum 要求至少 64 位熵）。
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("生成序列号失败: %w", err)
	}
	return serial, nil
}

// formatFingerprint 转成 "AA:BB:CC..." 形式，方便用户与证书详情做人工比对。
func formatFingerprint(sum []byte) string {
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// publicKeyEqual 比较两个公钥是否一致，用于校验证书与私钥的配对关系。
func publicKeyEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	if e, ok := a.(equaler); ok {
		return e.Equal(b)
	}
	return false
}
