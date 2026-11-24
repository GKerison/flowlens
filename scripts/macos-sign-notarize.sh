#!/usr/bin/env bash
# 给 macOS 产物签名并提交公证。
#
# 用法：
#   export SIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
#   export APPLE_ID="you@example.com"
#   export APPLE_TEAM_ID="TEAMID"
#   export APPLE_APP_PASSWORD="abcd-efgh-ijkl-mnop"   # App 专用密码，不是登录密码
#   bash scripts/macos-sign-notarize.sh bin/flowlens.app
#
# 为什么必须做完"公证"这一步：从 macOS 10.15 起，未公证的 App 双击会提示
# "无法验证开发者"，用户只能在"系统设置 → 隐私与安全性"里手动放行，
# 而 FlowLens 的核心功能是改系统代理，没人愿意给一个来源不明的程序授权。
set -euo pipefail

APP_PATH="${1:-bin/flowlens.app}"
ENTITLEMENTS="build/darwin/entitlements.plist"
APP_NAME="$(basename "${APP_PATH}" .app)"
ZIP_PATH="bin/${APP_NAME}.zip"

die() { echo "❌ $*" >&2; exit 1; }

[[ -d "${APP_PATH}" ]] || die "找不到 .app：${APP_PATH}（先执行 wails3 task darwin:package）"
: "${SIGN_IDENTITY:?请设置 SIGN_IDENTITY（security find-identity -v -p codesigning 可查）}"
[[ -f "${ENTITLEMENTS}" ]] || die "缺少 entitlements：${ENTITLEMENTS}"
mkdir -p bin

echo "▸ 签名（Hardened Runtime + 时间戳）"
# --options runtime 是公证的硬性要求；--timestamp 保证签名在证书有效期内长期可校验
codesign --force --deep --options runtime --timestamp \
  --entitlements "${ENTITLEMENTS}" \
  --sign "${SIGN_IDENTITY}" "${APP_PATH}"

echo "▸ 校验签名"
codesign --verify --deep --strict --verbose=2 "${APP_PATH}"
# Gatekeeper 的静态评估：这里能过，用户下载后才不会被拦
spctl --assess --type execute --verbose=2 "${APP_PATH}" || \
  echo "⚠️  spctl 未通过（公证完成前属正常现象）"

if [[ -z "${APPLE_ID:-}" || -z "${APPLE_TEAM_ID:-}" || -z "${APPLE_APP_PASSWORD:-}" ]]; then
  echo "⚠️  未提供 APPLE_ID / APPLE_TEAM_ID / APPLE_APP_PASSWORD，跳过公证"
  echo "   产物已签名，但用户首次打开仍需要手动放行。"
  exit 0
fi

echo "▸ 打包上传（notarytool 只接受 zip/dmg/pkg）"
/usr/bin/ditto -c -k --keepParent "${APP_PATH}" "${ZIP_PATH}"

echo "▸ 提交公证并等待结果"
xcrun notarytool submit "${ZIP_PATH}" \
  --apple-id "${APPLE_ID}" \
  --team-id "${APPLE_TEAM_ID}" \
  --password "${APPLE_APP_PASSWORD}" \
  --wait

echo "▸ 装订公证票据（离线也能通过 Gatekeeper）"
xcrun stapler staple "${APP_PATH}"
xcrun stapler validate "${APP_PATH}"

echo "✅ 完成：${APP_PATH}"
