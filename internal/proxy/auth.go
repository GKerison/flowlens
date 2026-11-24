package proxy

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

// AuthRealm 是 407 响应里的认证域名称，客户端登录框会把它展示给用户。
const AuthRealm = "观流"

// authFunc 生成代理认证回调（HTTP Basic）。
//
// 三个必须做对的细节：
//  1. 凭据在 Proxy-Authorization 头里，不是 Authorization
//     （后者是源站凭据，req.BasicAuth() 只读它，所以不能直接用）。
//  2. 失败时返回的 error 不能为 nil —— 内核会直接 err.Error() 打日志，nil 会 panic。
//  3. 比较用常量时间，避免通过响应时间差逐字节猜出密码。
//
// 回调始终安装、每次请求现读配置：这样在设置里开关认证或改密码立即生效，
// 不需要重启代理（重启会掐断正在抓的连接）。
func (m *Manager) authFunc() func(http.ResponseWriter, *http.Request) (bool, error) {
	return func(w http.ResponseWriter, req *http.Request) (bool, error) {
		cfg := m.cfg.Get()
		if !cfg.RequireAuth {
			return true, nil
		}
		if checkProxyAuth(req, cfg.AuthUsername, cfg.AuthPassword) {
			return true, nil
		}
		// 内核在返回 407 时会自己补一个 Proxy-Authenticate（realm 固定为 "proxy"），
		// 这里先设一份自定义 realm：万一内核改了这段逻辑，客户端仍能弹出登录框。
		w.Header().Set("Proxy-Authenticate", `Basic realm="`+AuthRealm+`", charset="UTF-8"`)
		return false, errors.New("代理认证失败：用户名或密码不正确")
	}
}

// checkProxyAuth 校验请求头里的 Basic 凭据。
//
// 未配置密码时一律拒绝：宁可用户连不上，也不要一个空密码的代理对局域网敞开。
func checkProxyAuth(req *http.Request, wantUser, wantPass string) bool {
	if wantPass == "" {
		return false
	}
	user, pass, ok := proxyBasicAuth(req)
	if !ok {
		return false
	}
	// 两个比较都要执行完：&& 短路会让"用户名错"比"密码错"更快返回，泄露信息
	uOK := subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(pass), []byte(wantPass)) == 1
	return uOK && pOK
}

// proxyBasicAuth 从 Proxy-Authorization 解析用户名密码。
// 兼容少数把凭据塞进 Authorization 的客户端（例如部分命令行工具）。
func proxyBasicAuth(req *http.Request) (username, password string, ok bool) {
	raw := req.Header.Get("Proxy-Authorization")
	if raw == "" {
		raw = req.Header.Get("Authorization")
	}
	if raw == "" {
		return "", "", false
	}
	scheme, encoded, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", false
	}
	return user, pass, true
}
