module github.com/kerison/flowlens

go 1.27.0

require (
	github.com/gorilla/websocket v1.5.3
	github.com/lqqyt2423/go-mitmproxy v1.9.5
	github.com/sirupsen/logrus v1.9.4
	github.com/wailsapp/wails/v3 v3.1.0
	golang.org/x/sys v0.49.0
	golang.org/x/text v0.43.0
	software.sslmate.com/src/go-pkcs12 v0.7.4
)

// 本地化内核仅改一处：惰性 HTTPS 握手不再固定声明 http/1.1，
// 让只提供 h2 的客户端（iPhone Safari）回落 HTTP/1.1。详见 CONVENTIONS.md。
replace github.com/lqqyt2423/go-mitmproxy => ./third_party/go-mitmproxy

require (
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/ebitengine/purego v0.11.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/term v0.47.0 // indirect
)
