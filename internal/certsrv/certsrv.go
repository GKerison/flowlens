// Package certsrv 提供「手机/其他设备怎么拿到根证书」的局域网分发服务。
//
// 它只做三件事，且全部是**只读、公开信息**：
//   - `/`                手机友好的下载页（平台指引 + 证书摘要 + 下载入口）
//   - `/ca.crt` `ca.pem` 根证书本体（DER / PEM）
//   - `/ca.mobileconfig` iOS / macOS 描述文件（装完即进入「已下载描述文件」）
//
// 安全边界：
//   - 不提供任何管理接口，不暴露私钥，不读取会话数据；
//   - 仅在用户显式把监听地址切到 0.0.0.0（局域网模式）时才对其他设备可见；
//   - 每次下载都写日志（含来源 IP），便于用户核对"是谁装的"。
package certsrv

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kerison/flowlens/internal/applog"
)

// Options 是启动证书服务所需的全部输入。
type Options struct {
	Host        string    // 监听地址，与代理保持一致（127.0.0.1 或 0.0.0.0）
	Port        int       // 监听端口
	CertPEM     []byte    // 根证书 PEM
	CertDER     []byte    // 根证书 DER，用于 mobileconfig 的 <data>
	CommonName  string    // 证书 CN
	Fingerprint string    // SHA-256 指纹（冒号分隔），页面展示用
	NotAfter    time.Time // 到期时间
	Version     string    // 应用版本，页面底部展示
}

// Server 是证书分发 HTTP 服务。可重复 Start/Stop。
type Server struct {
	opts Options

	mu      sync.RWMutex
	httpSrv *http.Server
	addr    string
}

// New 创建服务对象，不启动监听。
func New(opts Options) *Server {
	return &Server{opts: opts}
}

// Start 启动监听。已在运行时直接返回当前地址。
func (s *Server) Start() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpSrv != nil {
		return s.addr, nil
	}
	if len(s.opts.CertDER) == 0 || len(s.opts.CertPEM) == 0 {
		return "", errors.New("证书服务缺少根证书内容")
	}

	host := s.opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(s.opts.Port))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("证书服务无法监听 %s: %w（端口被占用？）", addr, err)
	}

	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// 单次请求很快，给 10s 足够；避免慢连接长期占用
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	s.httpSrv = srv
	s.addr = ln.Addr().String()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			applog.Warnf("证书服务异常退出: %v", err)
		}
	}()
	applog.Infof("证书下载页已启动：http://%s", s.addr)
	return s.addr, nil
}

// Stop 停止监听。未启动时返回 nil。
func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.httpSrv
	s.httpSrv = nil
	s.addr = ""
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("停止证书服务失败: %w", err)
	}
	applog.Infof("证书下载页已停止")
	return nil
}

// Addr 返回当前监听地址（未运行为空）。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr
}

// Running 表示服务是否在监听。
func (s *Server) Running() bool { return s.Addr() != "" }

// handler 组装路由。刻意保持扁平：只有四个公开资源，多一层路由库不值得。
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/ca.crt", s.handleDER)
	mux.HandleFunc("/ca.pem", s.handlePEM)
	mux.HandleFunc("/ca.mobileconfig", s.handleMobileconfig)
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("flowlens"))
	})
	return mux
}

// handlePage 渲染下载页。除根路径外一律 404，避免暴露目录结构。
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := renderPage(w, s.pageData(r)); err != nil {
		applog.Warnf("渲染证书下载页失败: %v", err)
	}
}

// handleDER 返回 DER 格式证书。
// 用 application/x-x509-ca-cert 而不是通用二进制类型：iOS/Android/Windows
// 都据此直接弹出「安装证书」而不是「下载文件」。
func (s *Server) handleDER(w http.ResponseWriter, r *http.Request) {
	s.logDownload(r, "ca.crt")
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="flowlens-ca.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.opts.CertDER)
}

// handlePEM 返 PEM 格式证书，方便脚本 curl 后直接给系统工具使用。
func (s *Server) handlePEM(w http.ResponseWriter, r *http.Request) {
	s.logDownload(r, "ca.pem")
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="flowlens-ca.pem"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.opts.CertPEM)
}

// handleMobileconfig 返回 iOS/macOS 描述文件。
// iOS 必须走这一步才能"一次点击"把证书装进「已下载描述文件」，
// 否则用户要在设置里手动导入，失败率很高。
func (s *Server) handleMobileconfig(w http.ResponseWriter, r *http.Request) {
	s.logDownload(r, "ca.mobileconfig")
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", `attachment; filename="flowlens-ca.mobileconfig"`)
	w.Header().Set("Cache-Control", "no-store")
	body, err := buildMobileconfig(s.opts)
	if err != nil {
		http.Error(w, "生成描述文件失败", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// logDownload 记录谁下载了证书，供用户核对。
func (s *Server) logDownload(r *http.Request, file string) {
	applog.Infof("证书下载：%s 来自 %s", file, clientIP(r))
}

// clientIP 取真实来源 IP，去掉端口。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// pageData 汇总下载页需要展示的信息。
func (s *Server) pageData(r *http.Request) pageModel {
	port := s.opts.Port
	host := clientIP(r)
	// 下载页展示的地址用"用户实际访问的地址"，这样手机上看到的就是它能用的地址
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))

	return pageModel{
		CommonName:    s.opts.CommonName,
		Fingerprint:   s.opts.Fingerprint,
		NotAfter:      s.opts.NotAfter.Format("2006-01-02"),
		BaseURL:       base,
		Version:       s.opts.Version,
		ShortFinger:   shortFingerprint(s.opts.Fingerprint),
		ShortFingerID: profileUUID(s.opts.CertDER),
	}
}

// shortFingerprint 取 SHA-256 前 4 组，页面上够辨认、又不至于刷屏。
func shortFingerprint(fp string) string {
	parts := strings.Split(fp, ":")
	if len(parts) < 4 {
		return fp
	}
	return strings.Join(parts[:4], ":") + " …"
}

// profileUUID 由证书内容派生固定 UUID。
// 固定值的好处：重复下载描述文件是"覆盖"而不是装出一堆同名描述文件。
func profileUUID(der []byte) string {
	sum := sha256.Sum256(der)
	b := sum[:16]
	// 按 UUID v4 的格式呈现（设置版本位与变体位，避免某些解析器抱怨）
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// base64DER 把 DER 转成 plist <data> 需要的一行 base64。
func base64DER(der []byte) string {
	return base64.StdEncoding.EncodeToString(der)
}
