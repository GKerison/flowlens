package service

import "strings"

// Version 由构建流程通过 -ldflags 注入。
// git tag 形如 v0.1.0，界面展示时统一去掉前缀 v，避免出现 "vv0.1.0"。
// 默认值只用于本地开发。
var Version = "0.1.0"

// VersionNumber 返回不带 v 前缀、去掉首尾空白的版本号。
// 所有展示与版本比较都走这里，保证 tag 命名习惯不影响界面。
func VersionNumber() string {
	v := strings.TrimSpace(Version)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}
