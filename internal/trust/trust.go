// Package trust 负责把 观流 根证书装进（或移出）系统信任库。
//
// 设计原则：
//  1. **透明**：所有会执行的命令都以字符串形式返回给界面，用户能看到"到底要干什么"，
//     再决定是否同意。这是本应用唯一需要改动系统安全配置的模块。
//  2. **可撤销**：安装与卸载成对提供，卸载只删我们自己的证书（按指纹/公用名精确匹配），
//     不会波及用户已有的其他根证书。
//  3. **最小权限**：优先使用用户级信任库（无需管理员），仅在系统级库必要时才提权；
//     提权一律交给系统原生弹窗（macOS osascript / Linux pkexec），不自己弹密码框。
//  4. **失败可解释**：返回的错误要带上平台细节（命令、退出码、stderr），否则用户只会看到
//     "安装失败"四个字，无从下手。
//
// 平台差异被收敛到 trust_<os>.go 里的三个函数：detect/install/uninstall。
package trust

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Store 描述证书将被安装到哪个信任库，供界面提示"影响范围"。
type Store struct {
	Name     string `json:"name"`     // 例如「用户登录钥匙串」「受信任的根证书颁发机构（当前用户）」
	Scope    string `json:"scope"`    // user | system
	Requires string `json:"requires"` // 空表示无需额外权限；否则是提示文案，如「需要管理员授权」
}

// Status 是信任状态的检测结果。
type Status struct {
	Trusted     bool      `json:"trusted"`     // 是否已在本机信任库中找到这张证书
	Elevated    bool      `json:"elevated"`    // 若已信任，是否装在系统级（而非用户级）
	Detail      string    `json:"detail"`      // 面向用户的一句话说明
	Store       Store     `json:"store"`       // 默认安装目标
	CanElevate  bool      `json:"canElevate"`  // 当前平台是否支持提权安装（失败后可重试）
	Command     []string  `json:"command"`     // 默认安装将执行的命令，供预览
	Fingerprint string    `json:"fingerprint"` // SHA-256，便于用户与系统界面比对
	CheckedAt   time.Time `json:"checkedAt"`
}

// Result 是安装/卸载的执行结果。
type Result struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message"`
	Command []string `json:"command"`
	Output  string   `json:"output"` // 命令输出，失败时尤其有用
}

// Manager 持有根证书信息，用于检测与安装。
// 只持有公钥证书：安装信任不需要私钥，避免私钥被带进子进程环境。
type Manager struct {
	certPath string // PEM 文件绝对路径
	cert     *x509.Certificate
}

// New 创建信任管理器。certPath 必须是 PEM 格式的根证书文件。
func New(certPath string) (*Manager, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("读取根证书失败: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("根证书不是有效的 PEM 格式")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析根证书失败: %w", err)
	}
	return &Manager{certPath: certPath, cert: cert}, nil
}

// CertPath 返回证书文件路径。
func (m *Manager) CertPath() string { return m.certPath }

// CommonName 返回根证书的 CN，用于在系统信任库中定位。
func (m *Manager) CommonName() string { return m.cert.Subject.CommonName }

// SHA256 返回冒号分隔的 SHA-256 指纹（与 ca.Info 展示的一致）。
func (m *Manager) SHA256() string {
	sum := sha256.Sum256(m.cert.Raw)
	return colonHex(sum[:])
}

// SHA1 返回大写的 SHA-1 指纹（不带分隔符）。
// Windows 的 certutil 与 macOS 的 security 在按指纹操作时都用 SHA-1，故单独提供。
func (m *Manager) SHA1() string {
	sum := sha1.Sum(m.cert.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// Status 检测当前平台是否已信任本证书。
func (m *Manager) Status() Status {
	trusted, elevated, detail, cmd := detect(m)
	return Status{
		Trusted:     trusted,
		Elevated:    elevated,
		Detail:      detail,
		Store:       currentStore(),
		CanElevate:  canElevate(),
		Command:     cmd,
		Fingerprint: m.SHA256(),
		CheckedAt:   time.Now(),
	}
}

// Install 以默认方式安装（优先用户级，无需管理员密码）。
// 调用前界面应已展示 Status.Command 并取得用户同意。
func (m *Manager) Install() (Result, error) { return install(m, false) }

// InstallElevated 以系统级方式安装（会弹出系统授权弹窗）。
// 仅在默认安装失败、或用户明确要求"对所有用户生效"时使用。
func (m *Manager) InstallElevated() (Result, error) { return install(m, true) }

// Uninstall 从系统信任库移除本证书。
// 只按本证书的标识删除，不影响用户其他根证书。
func (m *Manager) Uninstall() (Result, error) { return uninstall(m) }

// colonHex 把字节切片格式化成 "AA:BB:CC" 形式。
func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, ":")
}

// runCmd 执行命令并返回合并输出。
// 统一入口便于记录日志、统一超时行为；不吞掉错误输出，失败时把它交给调用方。
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("%s 执行失败: %w", name, err)
		}
		return text, fmt.Errorf("%s 执行失败: %w（%s）", name, err, firstLine(text))
	}
	return text, nil
}

// firstLine 取输出的第一行，避免把整段 stderr 塞进错误信息。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// errNotApplied 表示命令返回成功，但重新检测仍未见证书生效。
// 常见原因：系统策略拦截、证书被写入其他用户上下文、pc 上有同名旧证书。
var errNotApplied = errors.New("命令已执行，但系统信任库中未检测到该证书，请打开系统证书管理器确认")
