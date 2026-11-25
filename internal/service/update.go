// 版本检测与一键更新。
//
// 安全边界：
//   - 版本信息只从项目自己的 GitHub Releases 或 GitHub Pages 读取；
//   - 下载只接受白名单域名，且限制大小，绝不执行用户传入的任意 URL；
//   - 安装动作全部在后端完成，前端只负责展示结果与触发"更新并重启"。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kerison/flowlens/internal/applog"
)

const (
	updateRepo        = "GKerison/flowlens"
	updateAPIURL      = "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	updateFallbackURL = "https://gkerison.github.io/flowlens/version.json"
	updatePageURL     = "https://github.com/" + updateRepo + "/releases/latest"
	updateTimeout     = 15 * time.Second
	// 单个安装包上限 1 GiB，防止异常响应把磁盘写满。
	maxUpdateAssetBytes = int64(1 << 30)
)

// UpdateInfo 是"检查更新"的返回内容。字段与前端 types.ts 一一对应。
type UpdateInfo struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	Notes          string `json:"notes"`
	PublishedAt    string `json:"publishedAt"`
	AssetName      string `json:"assetName"`
	AssetURL       string `json:"assetUrl"`
	AssetSize      int64  `json:"assetSize"`
	PageURL        string `json:"pageUrl"`
	UpToDate       bool   `json:"upToDate"`
	CheckedAt      string `json:"checkedAt"`
}

// UpdateResult 是"下载并安装更新"的返回内容。
type UpdateResult struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	Path       string `json:"path"`
	Restarting bool   `json:"restarting"`
	// Manual=true 表示无法静默安装，安装包已落盘并打开了所在目录，需要用户手动完成。
	Manual bool `json:"manual"`
}

// updateAsset 是 Release 里的一个可下载产物。
type updateAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// githubRelease 只解析 CheckUpdate 需要的字段，避免绑定生成器暴露第三方类型。
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Assets      []updateAsset `json:"assets"`
}

// versionManifest 是 GitHub Pages 上的备用版本清单。
// GitHub API 受限时仍能提示用户有新版本。
type versionManifest struct {
	Version     string                 `json:"version"`
	Notes       string                 `json:"notes"`
	PublishedAt string                 `json:"publishedAt"`
	PageURL     string                 `json:"pageUrl"`
	Assets      map[string]updateAsset `json:"assets"`
}

// latestVersion 是两种来源归一化后的结果。
type latestVersion struct {
	version     string
	notes       string
	publishedAt string
	pageURL     string
	assets      []updateAsset
}

// quitFn 由 main 注入 Wails 的 App.Quit。更新安装成功后延后调用，
// 让前端先收到"更新成功，正在重启"的提示。
var quitFn func()

// SetQuitter 由 main 在应用实例就绪后调用。
func SetQuitter(fn func()) { quitFn = fn }

// CheckUpdate 查询最新版本。网络失败时返回带上下文的错误，前端原样展示。
func (a *App) CheckUpdate() (UpdateInfo, error) {
	current := VersionNumber()
	info := UpdateInfo{CurrentVersion: current, PageURL: updatePageURL, CheckedAt: time.Now().Format(time.RFC3339)}

	latest, err := fetchLatestVersion()
	if err != nil {
		return info, err
	}
	info.LatestVersion = VersionNumberFrom(latest.version)
	info.Notes = latest.notes
	info.PublishedAt = latest.publishedAt
	if latest.pageURL != "" {
		info.PageURL = latest.pageURL
	}

	if compareVersions(info.LatestVersion, current) <= 0 {
		info.UpToDate = true
		return info, nil
	}

	asset := pickUpdateAsset(latest.assets, runtime.GOOS, runtime.GOARCH)
	if asset == nil {
		return info, fmt.Errorf("发现新版本 %s，但当前平台（%s/%s）暂无可自动安装的产物，请到发布页下载", info.LatestVersion, runtime.GOOS, runtime.GOARCH)
	}
	info.AssetName = asset.Name
	info.AssetURL = asset.URL
	info.AssetSize = asset.Size
	return info, nil
}

// InstallUpdate 下载并静默安装最新版本，成功后自动重启应用。
//
// 失败时不会静默吞掉：会把安装包留在下载目录、打开所在目录并返回 Manual=true，
// 用户仍可手动完成安装，不会白等一次"更新"。
func (a *App) InstallUpdate() (UpdateResult, error) {
	info, err := a.CheckUpdate()
	if err != nil {
		return UpdateResult{}, err
	}
	if info.UpToDate {
		return UpdateResult{OK: true, Message: "已是最新版本，无需更新"}, nil
	}
	if err := validateUpdateURL(info.AssetURL); err != nil {
		return UpdateResult{}, err
	}

	path, size, err := downloadUpdateAsset(info)
	if err != nil {
		return UpdateResult{}, err
	}
	applog.Infof("更新包已下载：%s（%d 字节）", path, size)

	result, err := installUpdateAsset(path, info.AssetName)
	if err != nil {
		applog.Warnf("自动安装更新失败：%v", err)
		openFileLocation(path)
		return UpdateResult{
			OK:      false,
			Manual:  true,
			Path:    path,
			Message: fmt.Sprintf("更新包已下载到 %s，但无法自动安装（%v）。已打开所在目录，请手动安装后重启。", path, err),
		}, nil
	}

	result.Path = path
	result.Restarting = quitFn != nil
	if result.Restarting {
		scheduleAppQuit()
	}
	return result, nil
}

/* ---------------- 版本比较 ---------------- */

type parsedVersion struct {
	major, minor, patch int
	pre                 bool
}

// VersionNumberFrom 把 tag（v0.1.0 / 0.1.0-rc.1）转成纯版本号。
func VersionNumberFrom(raw string) string {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// parseVersion 解析 x[.y[.z]][-prerelease]。缺少的段按 0 处理。
func parseVersion(raw string) (parsedVersion, bool) {
	v := VersionNumberFrom(raw)
	if v == "" {
		return parsedVersion{}, false
	}
	pre := false
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre = true
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return parsedVersion{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return parsedVersion{}, false
		}
		nums[i] = n
	}
	return parsedVersion{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

// compareVersions 比较版本号：a 新返回 1，a 旧返回 -1，相同返回 0。
// 预发布版本（含 -rc / -beta）低于同号的正式版本。
func compareVersions(a, b string) int {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		// 无法解析的版本不参与升级判断，宁可停在旧版本。
		return 0
	}
	for _, pair := range [][2]int{{av.major, bv.major}, {av.minor, bv.minor}, {av.patch, bv.patch}} {
		if pair[0] > pair[1] {
			return 1
		}
		if pair[0] < pair[1] {
			return -1
		}
	}
	switch {
	case av.pre && !bv.pre:
		return -1
	case !av.pre && bv.pre:
		return 1
	}
	return 0
}

/* ---------------- 远端查询 ---------------- */

func fetchLatestVersion() (latestVersion, error) {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	release, err := fetchGitHubRelease(ctx)
	if err == nil {
		notes := strings.TrimSpace(release.Body)
		if notes == "" {
			notes = strings.TrimSpace(release.Name)
		}
		return latestVersion{
			version:     release.TagName,
			notes:       notes,
			publishedAt: release.PublishedAt,
			pageURL:     release.HTMLURL,
			assets:      release.Assets,
		}, nil
	}
	applog.Warnf("GitHub Releases 查询失败，尝试备用版本清单：%v", err)

	manifest, ferr := fetchVersionManifest(ctx)
	if ferr != nil {
		return latestVersion{}, fmt.Errorf("检查更新失败：GitHub API（%v）；备用清单（%v）", err, ferr)
	}
	assets := make([]updateAsset, 0, len(manifest.Assets))
	for _, key := range []string{runtime.GOOS, "default"} {
		if asset, ok := manifest.Assets[key]; ok && asset.URL != "" {
			assets = append(assets, asset)
		}
	}
	return latestVersion{
		version:     manifest.Version,
		notes:       manifest.Notes,
		publishedAt: manifest.PublishedAt,
		pageURL:     manifest.PageURL,
		assets:      assets,
	}, nil
}

func fetchGitHubRelease(ctx context.Context) (githubRelease, error) {
	var out githubRelease
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateAPIURL, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "guanliu-updater/"+VersionNumber())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	if out.TagName == "" {
		return out, errors.New("发布信息缺少版本号")
	}
	return out, nil
}

func fetchVersionManifest(ctx context.Context) (versionManifest, error) {
	var out versionManifest
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateFallbackURL, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("User-Agent", "guanliu-updater/"+VersionNumber())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	if out.Version == "" {
		return out, errors.New("版本清单缺少版本号")
	}
	return out, nil
}

/* ---------------- 产物选择 ---------------- */

// assetScore 给产物打分：小于 0 表示不适用，越大越优先。
// 打分只依据文件名，不执行任何下载内容。
func assetScore(name, goos, goarch string) int {
	n := strings.ToLower(name)
	arch64 := goarch == "amd64" || goarch == "x86_64"
	arm := goarch == "arm64"

	switch goos {
	case "darwin":
		if !strings.HasSuffix(n, ".dmg") {
			return -1
		}
		score := 10
		if strings.Contains(n, "universal") {
			score += 6
		} else if arm && strings.Contains(n, "arm64") {
			score += 5
		} else if arch64 && (strings.Contains(n, "amd64") || strings.Contains(n, "x64")) {
			score += 5
		}
		return score
	case "windows":
		if !strings.HasSuffix(n, ".exe") {
			return -1
		}
		score := 10
		if strings.Contains(n, "x64") || strings.Contains(n, "amd64") {
			score += 5
		} else if arm && strings.Contains(n, "arm64") {
			score += 4
		}
		if strings.Contains(n, "arm64") && !arm {
			return -1
		}
		return score
	case "linux":
		score := -1
		switch {
		case strings.HasSuffix(n, ".appimage"):
			score = 30
		case strings.HasSuffix(n, ".deb"):
			score = 20
		case strings.HasSuffix(n, ".rpm"):
			score = 10
		default:
			return -1
		}
		if strings.Contains(n, "arm64") && !arm {
			return -1
		}
		if arch64 && (strings.Contains(n, "x64") || strings.Contains(n, "amd64")) {
			score++
		}
		return score
	default:
		return -1
	}
}

func pickUpdateAsset(assets []updateAsset, goos, goarch string) *updateAsset {
	best := -1
	bestScore := -1
	for i := range assets {
		score := assetScore(assets[i].Name, goos, goarch)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return nil
	}
	return &assets[best]
}

/* ---------------- 下载与安装 ---------------- */

func validateUpdateURL(raw string) error {
	if raw == "" {
		return errors.New("更新地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("更新地址不合法：%w", err)
	}
	if u.Scheme != "https" {
		return errors.New("更新地址必须使用 HTTPS")
	}
	host := strings.ToLower(u.Hostname())
	allowed := []string{
		"github.com",
		"api.github.com",
		"objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
		"gkerison.github.io",
	}
	for _, h := range allowed {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("更新地址不在信任域名内：%s", host)
}

func downloadUpdateAsset(info UpdateInfo) (string, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.AssetURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "guanliu-updater/"+VersionNumber())
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("下载更新包失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("下载更新包失败：HTTP %d", resp.StatusCode)
	}

	ext := filepath.Ext(info.AssetName)
	if ext == "" {
		ext = ".bin"
	}
	tmp, err := os.CreateTemp("", "guanliu-update-*"+ext)
	if err != nil {
		return "", 0, err
	}
	path := tmp.Name()
	cleanup := func() { _ = os.Remove(path) }

	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxUpdateAssetBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return "", 0, fmt.Errorf("写入更新包失败：%w", err)
	}
	if n == 0 || n > maxUpdateAssetBytes {
		cleanup()
		return "", 0, fmt.Errorf("更新包大小异常：%d 字节", n)
	}
	// 服务端声明了长度却对不上，说明下载不完整，绝不能拿去安装。
	if resp.ContentLength > 0 && n != resp.ContentLength {
		cleanup()
		return "", 0, fmt.Errorf("更新包不完整：期望 %d 字节，实际 %d 字节", resp.ContentLength, n)
	}
	return path, n, nil
}

func installUpdateAsset(path, assetName string) (UpdateResult, error) {
	switch runtime.GOOS {
	case "darwin":
		return installMacOSUpdate(path)
	case "windows":
		return installWindowsUpdate(path)
	case "linux":
		return installLinuxUpdate(path, assetName)
	default:
		return UpdateResult{}, fmt.Errorf("平台 %s 暂不支持自动安装", runtime.GOOS)
	}
}

// macAppDestination 定位当前运行的 .app；不在标准目录时安装到 ~/Applications。
func macAppDestination() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := exe
	for i := 0; i < 12; i++ {
		if strings.HasSuffix(dir, ".app") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Applications", "观流.app"), nil
}

func findMountPoint(output string) string {
	var found string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "/Volumes/") {
			continue
		}
		fields := strings.Fields(line)
		for _, f := range fields {
			if strings.HasPrefix(f, "/Volumes/") && strings.Count(f, "/") >= 2 {
				found = f
			}
		}
	}
	return found
}

func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installMacOSUpdate(dmg string) (UpdateResult, error) {
	out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-noautoopen", dmg).CombinedOutput()
	if err != nil {
		return UpdateResult{}, fmt.Errorf("挂载 DMG 失败：%s", strings.TrimSpace(string(out)))
	}
	mount := findMountPoint(string(out))
	if mount == "" {
		return UpdateResult{}, errors.New("无法定位 DMG 挂载目录")
	}
	defer func() { _ = exec.Command("hdiutil", "detach", mount, "-force").Run() }()

	apps, err := filepath.Glob(filepath.Join(mount, "*.app"))
	if err != nil || len(apps) == 0 {
		return UpdateResult{}, errors.New("DMG 中未找到应用")
	}
	sort.Strings(apps)
	dest, err := macAppDestination()
	if err != nil {
		return UpdateResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return UpdateResult{}, err
	}
	if err := runCommand("rsync", "-a", "--delete", apps[0]+"/", dest+"/"); err != nil {
		return UpdateResult{}, fmt.Errorf("替换应用失败：%w", err)
	}
	// 从 DMG 复制过来的应用会带 quarantine 属性，去掉后不会再弹"来自未知开发者"。
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", dest).Run()
	if err := exec.Command("open", "-n", dest).Start(); err != nil {
		return UpdateResult{}, fmt.Errorf("重启应用失败：%w", err)
	}
	return UpdateResult{OK: true, Message: "更新完成，正在重启观流"}, nil
}

// windowsInstallDirs 按注册表 → 常见目录的顺序给出安装位置。
func windowsInstallDirs() []string {
	var dirs []string
	// 新版显示为「观流」，旧版安装包注册名仍是 FlowLens；两者都要能找到。
	for _, root := range []string{"HKCU", "HKLM"} {
		for _, key := range []string{`观流观流`, `KerisonFlowLens`, `FlowLens`} {
			out, err := exec.Command("reg", "query", root+`\Software\Microsoft\Windows\CurrentVersion\Uninstall\`+key, "/v", "InstallLocation").CombinedOutput()
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(out), "\n") {
				if idx := strings.Index(line, "REG_SZ"); idx >= 0 {
					d := strings.TrimSpace(line[idx+len("REG_SZ"):])
					if d != "" {
						dirs = append(dirs, d)
					}
				}
			}
		}
	}
	for _, base := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base == "" {
			continue
		}
		for _, leaf := range []string{
			filepath.Join("Programs", "观流"), filepath.Join("观流", "观流"), "观流",
			filepath.Join("Programs", "FlowLens"), filepath.Join("FlowLens", "FlowLens"), "FlowLens",
		} {
			dirs = append(dirs, filepath.Join(base, leaf))
		}
	}
	return dirs
}

/* ---------------- 安装（按平台） ---------------- */

func installWindowsUpdate(installer string) (UpdateResult, error) {
	// NSIS 的 /S 是静默安装；安装器自己会替换正在运行的旧文件。
	if err := runCommand(installer, "/S"); err != nil {
		return UpdateResult{}, fmt.Errorf("静默安装失败：%w", err)
	}
	exe := ""
	for _, dir := range windowsInstallDirs() {
		for _, name := range []string{"flowlens.exe", "观流.exe"} {
			candidate := filepath.Join(dir, name)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				exe = candidate
				break
			}
		}
		if exe != "" {
			break
		}
	}
	if exe == "" {
		return UpdateResult{}, errors.New("安装完成但找不到可执行文件，请从开始菜单启动")
	}
	// start 让新进程脱离当前终端，应用退出时不会被一起带走。
	if err := exec.Command("cmd", "/c", "start", "", `"`+exe+`"`).Start(); err != nil {
		return UpdateResult{}, fmt.Errorf("重启应用失败：%w", err)
	}
	return UpdateResult{OK: true, Message: "更新完成，正在重启观流"}, nil
}

func installLinuxUpdate(appImage, assetName string) (UpdateResult, error) {
	if !strings.HasSuffix(strings.ToLower(assetName), ".appimage") {
		return UpdateResult{}, fmt.Errorf("Linux 自动安装仅支持 AppImage，收到 %s", assetName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return UpdateResult{}, err
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return UpdateResult{}, err
	}
	dest := filepath.Join(binDir, "guanliu.AppImage")
	data, err := os.ReadFile(appImage)
	if err != nil {
		return UpdateResult{}, err
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return UpdateResult{}, err
	}
	if err := exec.Command(dest).Start(); err != nil {
		return UpdateResult{}, fmt.Errorf("重启应用失败：%w", err)
	}
	return UpdateResult{OK: true, Message: "更新完成，正在重启观流"}, nil
}

func scheduleAppQuit() {
	go func() {
		time.Sleep(900 * time.Millisecond)
		if quitFn != nil {
			quitFn()
		}
	}()
}

func openFileLocation(path string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", "-R", path).Start()
	case "windows":
		_ = exec.Command("explorer", "/select,", path).Start()
	default:
		_ = exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
}
