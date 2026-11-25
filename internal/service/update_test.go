package service

import "testing"

func TestVersionNumberFrom(t *testing.T) {
	cases := map[string]string{
		"v0.1.0":       "0.1.0",
		"0.2.3":        "0.2.3",
		" V1.4.0 ":     "1.4.0",
		"v0.3.0-rc.1":  "0.3.0-rc.1",
		"v0.1.0+build": "0.1.0+build",
	}
	for in, want := range cases {
		if got := VersionNumberFrom(in); got != want {
			t.Errorf("VersionNumberFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	good := []string{"0.1.0", "v1.2.3", "2", "1.4", "v0.3.0-rc.1"}
	for _, in := range good {
		if _, ok := parseVersion(in); !ok {
			t.Errorf("parseVersion(%q) 应成功", in)
		}
	}
	for _, in := range []string{"", "abc", "1.x.0", "1.2.3.4", "-1.0.0"} {
		if _, ok := parseVersion(in); ok {
			t.Errorf("parseVersion(%q) 应失败", in)
		}
	}
	if v, _ := parseVersion("v0.3.0-rc.1"); !v.pre {
		t.Error("预发布版本应标记 pre")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.1", "0.1.0", 1},
		{"0.2.0", "1.0.0", -1},
		{"1.10.0", "1.9.9", 1},
		{"0.3.0-rc.1", "0.3.0", -1},
		{"0.3.0", "0.3.0-rc.1", 1},
		{"bad", "0.1.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetScore(t *testing.T) {
	cases := []struct {
		name, goos, arch string
		want             int
	}{
		{"guanliu-universal.dmg", "darwin", "arm64", 16},
		{"guanliu-amd64-installer.exe", "windows", "amd64", 15},
		{"guanliu-arm64.AppImage", "linux", "arm64", 30},
		{"guanliu-arm64.AppImage", "linux", "amd64", -1},
		{"guanliu.deb", "linux", "amd64", 20},
		{"guanliu.exe", "darwin", "amd64", -1},
		{"guanliu.dmg", "windows", "amd64", -1},
	}
	for _, c := range cases {
		if got := assetScore(c.name, c.goos, c.arch); got != c.want {
			t.Errorf("assetScore(%q, %q, %q) = %d, want %d", c.name, c.goos, c.arch, got, c.want)
		}
	}
}

func TestPickUpdateAsset(t *testing.T) {
	assets := []updateAsset{
		{Name: "guanliu.deb", URL: "https://example.com/a.deb"},
		{Name: "guanliu-x64.AppImage", URL: "https://example.com/a.AppImage"},
		{Name: "guanliu-universal.dmg", URL: "https://example.com/a.dmg"},
	}
	got := pickUpdateAsset(assets, "linux", "amd64")
	if got == nil || got.Name != "guanliu-x64.AppImage" {
		t.Fatalf("Linux 应选择 AppImage，实际 %+v", got)
	}
	if got := pickUpdateAsset(assets, "darwin", "arm64"); got == nil || got.Name != "guanliu-universal.dmg" {
		t.Fatalf("macOS 应选择 DMG，实际 %+v", got)
	}
	if got := pickUpdateAsset(assets, "windows", "amd64"); got != nil {
		t.Fatalf("Windows 无产物时应返回 nil，实际 %+v", got)
	}
}

func TestValidateUpdateURL(t *testing.T) {
	for _, u := range []string{
		"https://github.com/GKerison/flowlens/releases/download/v0.2.0/guanliu.dmg",
		"https://objects.githubusercontent.com/a/b",
		"https://gkerison.github.io/flowlens/version.json",
	} {
		if err := validateUpdateURL(u); err != nil {
			t.Errorf("validateUpdateURL(%q) 不应失败：%v", u, err)
		}
	}
	for _, u := range []string{
		"",
		"http://github.com/a",
		"https://evil.example.com/update.exe",
		"javascript:alert(1)",
	} {
		if err := validateUpdateURL(u); err == nil {
			t.Errorf("validateUpdateURL(%q) 应失败", u)
		}
	}
}
