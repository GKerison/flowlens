//go:build !windows

package proxy_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
	"github.com/kerison/flowlens/internal/proxy"
)

// 这是一个"手动触发"的压测，不进 CI：默认跳过，需要时跑
//
//	FLOWLENS_LOADTEST=1 go test ./internal/proxy -run TestProxyLoad -v
//
// 目标是验证需求里的两条指标：100 req/s 持续压测下界面不卡、进程 CPU 占用可控。
// 之所以放在测试里而不是单独写个压测程序：能直接复用代理启动脚手架，
// 不引入 k6/wrk 这类外部依赖，也保证每次跑的环境完全一致。
func TestProxyLoad(t *testing.T) {
	if os.Getenv("FLOWLENS_LOADTEST") == "" {
		t.Skip("手动压测：设置 FLOWLENS_LOADTEST=1 后运行")
	}

	const (
		duration = 10 * time.Second
		workers  = 32 // 并发上限：模拟浏览器多标签页的并发量
	)
	// 默认压 100 req/s（需求里的指标），需要探上限时用 FLOWLENS_LOADTEST_RPS 调高
	rps := 100
	if v := os.Getenv("FLOWLENS_LOADTEST_RPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rps = n
		}
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"items":[1,2,3],"note":"load"}`))
	}))
	defer upstream.Close()

	// 自己装配而不复用 startProxyOpts：压测要一万级容量，而测试脚手架默认只留 100 条
	port := freePort(t)
	cfg, err := config.NewManagerAt(t.TempDir())
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.ListenHost = "127.0.0.1"
		c.ProxyPort = port
		c.MaxFlows = 10000
		return nil
	}); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	caMgr, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	store := capture.NewStore(10000)
	mgr := proxy.NewManager(cfg, caMgr, store, nil)
	if _, err := mgr.Start(); err != nil {
		t.Fatalf("启动代理失败: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	target := upstream.URL + "/api/items"

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(mustURL("http://" + addr)),
			MaxIdleConnsPerHost: workers,
		},
	}

	// 预热：把 TLS 握手、连接池、代码路径都跑热，避免把首次开销算进指标
	for i := 0; i < 50; i++ {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("预热请求失败: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	store.Clear()

	cpuBefore := readCPU()
	start := time.Now()

	var (
		mu       sync.Mutex
		latency  []time.Duration
		failures int
	)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()

	deadline := time.After(duration)
loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				reqStart := time.Now()
				resp, err := client.Get(target)
				if err != nil {
					mu.Lock()
					failures++
					mu.Unlock()
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					mu.Lock()
					failures++
					mu.Unlock()
					return
				}
				mu.Lock()
				latency = append(latency, time.Since(reqStart))
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(start)
	cpuUsed := readCPU() - cpuBefore

	sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
	pct := func(p float64) time.Duration {
		if len(latency) == 0 {
			return 0
		}
		idx := int(float64(len(latency)-1) * p)
		return latency[idx]
	}

	t.Logf("压测时长 %s，目标 %d req/s，实际完成 %d，成功 %d，失败 %d",
		elapsed.Round(time.Millisecond), rps, len(latency)+failures, len(latency), failures)
	t.Logf("实测吞吐 %.1f req/s；延迟 p50=%s p95=%s p99=%s max=%s",
		float64(len(latency))/elapsed.Seconds(),
		pct(0.50).Round(time.Microsecond), pct(0.95).Round(time.Microsecond),
		pct(0.99).Round(time.Microsecond), pct(1).Round(time.Microsecond))
	t.Logf("进程 CPU：%.2fs / %.2fs = %.1f%%（单核占比），会话入库 %d 条",
		cpuUsed.Seconds(), elapsed.Seconds(), cpuUsed.Seconds()/elapsed.Seconds()*100, store.Len())

	if failures > 0 {
		t.Fatalf("压测期间有 %d 个请求失败", failures)
	}
	// 高吞吐下会超过容量上限，此时入库数应恰好等于上限（最旧的被淘汰）
	wantStored := len(latency)
	if wantStored > 10000 {
		wantStored = 10000
	}
	if store.Len() != wantStored {
		t.Fatalf("入库条数 %d，期望 %d（成功 %d，上限 10000）", store.Len(), wantStored, len(latency))
	}
	if got := mgr.State(); !got.Running {
		t.Fatal("压测后代理应仍在运行")
	}
}

// readCPU 读取当前进程累计 CPU 时间（用户态 + 内核态）。
func readCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// mustURL 解析代理地址；地址是本文件拼的，解析失败说明代码写错了。
func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic("解析代理地址失败: " + err.Error())
	}
	return u
}
