// 观流（FlowLens）— 跨平台 HTTP/HTTPS 抓包工作台。
//
// 本文件只做三件事：装配依赖、创建窗口、注册前后端事件。
// 业务逻辑一律下沉到 internal/*，保持入口可读。
package main

import (
	"embed"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/kerison/flowlens/internal/applog"
	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/certsrv"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/events"
	"github.com/kerison/flowlens/internal/proxy"
	"github.com/kerison/flowlens/internal/service"
	"github.com/kerison/flowlens/internal/sysproxy"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	// 注册事件类型：绑定生成器据此产出强类型的 TS 接口，
	// 前端订阅事件时能获得类型提示，避免载荷字段改错。
	application.RegisterEvent[[]capture.FlowBrief](events.EventFlows)
	application.RegisterEvent[[]string](events.EventFlowsDeleted)
	application.RegisterEvent[capture.FrameBatch](events.EventFrames)
	application.RegisterEvent[proxy.State](events.EventProxyState)
}

func main() {
	if err := run(); err != nil {
		// 此时日志系统可能尚未就绪，直接写 stderr 保证用户能看到原因
		fmt.Fprintf(os.Stderr, "观流 启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// 1) 配置与日志：日志目录依赖配置目录，因此必须先加载配置
	cfg, err := config.NewManager()
	if err != nil {
		return err
	}
	if err := applog.Init(cfg.Dir()); err != nil {
		return err
	}
	defer applog.Close()
	applog.Infof("观流 启动，数据目录 %s", cfg.Dir())

	// 2) 证书：首次启动自动生成根证书，后续复用
	caMgr, err := ca.NewManager(filepath.Join(cfg.Dir(), "ca"))
	if err != nil {
		return fmt.Errorf("初始化证书失败: %w", err)
	}
	info := caMgr.Info()
	applog.Infof("根证书就绪：%s，有效期至 %s", info.CommonName, info.NotAfter.Format("2006-01-02"))

	// 3) 领域对象
	store := capture.NewStore(cfg.Get().MaxFlows)
	proxyMgr := proxy.NewManager(cfg, caMgr, store, nil)

	// 系统代理接管：目标永远是回环地址——即便代理监听在 0.0.0.0，
	// 本机流量也不该绕到局域网 IP 上。
	sysMgr := sysproxy.New(cfg.Dir(), func() string {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Get().ProxyPort))
	})

	// 启动自愈：上次没正常退出的话，系统代理可能还指着我们。
	boot := sysMgr.BootRecover()
	if boot.Note != "" {
		applog.Infof("系统代理自愈：%s", boot.Note)
	}
	if boot.Leftover {
		// 系统仍指向 观流 但上次没恢复成功：先把本地监听拉起来，
		// 避免用户在打开应用的瞬间发现"上不了网"。
		// 这只是启动自己的代理，不改任何系统设置。
		if _, err := proxyMgr.Start(); err != nil {
			applog.Warnf("残留接管期间启动代理失败：%v", err)
		}
	}

	// panic 也要尽量把系统代理还原，否则用户会带着一个死代理继续用电脑
	defer func() {
		if r := recover(); r != nil {
			if st := sysMgr.Status(); st.Enabled {
				applog.Errorf("发生 panic，尝试恢复系统代理：%v", r)
				_, _ = sysMgr.Disable()
			}
			panic(r)
		}
	}()

	// 终端 Ctrl+C / SIGTERM 不一定会走 OnShutdown，单独兜一层
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		applog.Infof("收到信号 %v，恢复系统代理后退出", s)
		if st := sysMgr.Status(); st.Enabled {
			if _, err := sysMgr.Disable(); err != nil {
				applog.Errorf("退出时恢复系统代理失败：%v", err)
			}
		}
		_ = proxyMgr.Stop()
		os.Exit(0)
	}()

	// 证书分发服务：手机扫码/浏览器打开就能下载证书与描述文件。
	// 与代理共用监听地址：只有用户显式切到 0.0.0.0（局域网模式）时才对外可见。
	certSrv := certsrv.New(certsrv.Options{
		Host:        cfg.Get().ListenHost,
		Port:        cfg.Get().CertPort,
		CertPEM:     []byte(info.PEM),
		CertDER:     caMgr.GetRootCA().Raw,
		CommonName:  info.CommonName,
		Fingerprint: info.Fingerprint,
		NotAfter:    info.NotAfter,
		Version:     service.Version,
	})
	if _, err := certSrv.Start(); err != nil {
		// 证书服务失败不该挡住抓包主流程：记录下来，界面会显示"证书服务未运行"
		applog.Warnf("证书分发服务启动失败：%v", err)
	}

	appSvc := service.New(service.Deps{
		Config:     cfg,
		CA:         caMgr,
		Store:      store,
		Proxy:      proxyMgr,
		CertServer: certSrv,
		SysProxy:   sysMgr,
		BootNote:   boot.Note,
	})

	// 4) 应用与窗口
	app := application.New(application.Options{
		Name:        "观流",
		Description: "跨平台 HTTP/HTTPS 抓包工作台",
		Services: []application.Service{
			application.NewService(appSvc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		OnShutdown: func() {
			// 退出前的顺序：先摘系统代理，再停监听。
			// 顺序反了会让系统指向一个已关闭的端口，退出瞬间就断网。
			applog.Infof("应用退出，恢复系统代理并停止代理")
			if st := sysMgr.Status(); st.Enabled {
				if _, err := sysMgr.Disable(); err != nil {
					applog.Errorf("退出时恢复系统代理失败（下次启动会提示一键恢复）：%v", err)
				}
			}
			_ = proxyMgr.Stop()
			_ = certSrv.Stop()
		},
	})

	emit := func(event string, payload any) { app.Event.Emit(event, payload) }
	proxyMgr.SetEmitter(emit)
	service.SetEmitter(emit)

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "观流",
		Width:            1280,
		Height:           820,
		MinWidth:         960,
		MinHeight:        620,
		BackgroundColour: application.NewRGB(247, 248, 250),
		URL:              "/",
	})

	if err := app.Run(); err != nil {
		log.Printf("应用运行失败: %v", err)
		return err
	}
	return nil
}
