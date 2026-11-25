#!/usr/bin/env node
/**
 * 自动生成版本号、更新日志与发布说明。
 *
 * 用法：
 *   node scripts/prepare-release.mjs                    # 根据 conventional commits 推断下一个版本
 *   node scripts/prepare-release.mjs --bump minor       # 强制次版本递增
 *   node scripts/prepare-release.mjs --tag v0.2.0       # 用指定 tag 作为版本（CI 传 tag 时用）
 *   node scripts/prepare-release.mjs --dry-run          # 只打印结果，不改文件
 *
 * 产物：
 *   CHANGELOG.md        追加本次发布段落（保留历史）
 *   RELEASE_NOTES.md    本次发布说明，供 GitHub Release 作为 body
 *   site/version.json   应用内版本检测的备用清单
 *   同步版本号到 build/config.yml、build/windows/info.json、MSIX 清单
 */
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync, existsSync, appendFileSync } from 'node:fs'
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const REPO = 'GKerison/flowlens'
const REPO_URL = `https://github.com/${REPO}`

function args() {
  const out = { version: '', tag: '', bump: 'auto', dryRun: false }
  const argv = process.argv.slice(2)
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a === '--version') out.version = argv[++i] || ''
    else if (a === '--tag') out.tag = argv[++i] || ''
    else if (a === '--bump') out.bump = argv[++i] || 'auto'
    else if (a === '--dry-run') out.dryRun = true
    else if (a === '--help' || a === '-h') {
      console.log('用法: prepare-release.mjs [--version X.Y.Z | --tag vX.Y.Z] [--bump auto|major|minor|patch] [--dry-run]')
      process.exit(0)
    } else throw new Error(`未知参数: ${a}`)
  }
  if (!['auto', 'major', 'minor', 'patch'].includes(out.bump)) {
    throw new Error(`--bump 必须是 auto/major/minor/patch，收到 ${out.bump}`)
  }
  return out
}

function git(...params) {
  try {
    return execFileSync('git', params, { cwd: ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()
  } catch {
    return ''
  }
}

function normalize(raw) {
  return String(raw || '').trim().replace(/^v/i, '')
}

function parseVersion(raw) {
  const m = /^(\d+)\.(\d+)\.(\d+)/.exec(normalize(raw))
  return m ? { major: +m[1], minor: +m[2], patch: +m[3] } : null
}

function configVersion() {
  const text = readFileSync(resolve(ROOT, 'build/config.yml'), 'utf8')
  const m = /info:[\s\S]*?version:\s*["']?([\d.]+)/.exec(text)
  return m ? m[1] : '0.1.0'
}

/** 决定基准版本：tag > 参数 > 最近 git tag > build/config.yml。 */
function baseVersion(opts) {
  if (opts.version) return normalize(opts.version)
  if (opts.tag) return normalize(opts.tag)
  const tag = git('describe', '--tags', '--abbrev=0')
  return tag ? normalize(tag) : configVersion()
}

function commitsSince(base) {
  const tag = git('describe', '--tags', '--abbrev=0')
  const range = tag ? `${tag}..HEAD` : 'HEAD'
  const raw = git('log', range, '--no-merges', '--pretty=%s%x1f%b%x1e')
  if (!raw) return []
  return raw
    .split('\x1e')
    .map((block) => block.trim())
    .filter(Boolean)
    .map((block) => {
      const [subject = '', body = ''] = block.split('\x1f')
      return { subject: subject.trim(), body: body.trim() }
    })
    .filter((c) => c.subject && !c.subject.startsWith('Merge '))
}

const GROUPS = [
  { key: 'breaking', title: '破坏性变更' },
  { key: 'feat', title: '新增' },
  { key: 'fix', title: '修复' },
  { key: 'perf', title: '性能优化' },
  { key: 'refactor', title: '重构' },
  { key: 'docs', title: '文档' },
  { key: 'chore', title: '构建与维护' },
  { key: 'other', title: '其他' },
]

function classify(commit) {
  const raw = `${commit.subject}\n${commit.body}`
  if (/\bBREAKING CHANGE\b|^[a-z]+!:/m.test(raw)) return 'breaking'
  const m = /^([a-z]+)(\([^)]+\))?[!]?:\s*(.+)$/.exec(commit.subject)
  if (!m) return 'other'
  const type = m[1]
  const text = m[3]
  if (['feat', 'fix', 'perf', 'refactor', 'docs'].includes(type)) return { key: type, text }
  if (['chore', 'ci', 'build', 'deps'].includes(type)) return { key: 'chore', text }
  if (['style', 'test', 'revert'].includes(type)) return { key: 'other', text }
  return { key: 'other', text: commit.subject }
}

function bumpVersion(base, commits, mode) {
  const v = parseVersion(base) || { major: 0, minor: 1, patch: 0 }
  let kind = mode
  if (mode === 'auto') {
    const kinds = commits.map(classify).map((c) => (typeof c === 'string' ? c : c.key))
    if (kinds.includes('breaking')) kind = 'major'
    else if (kinds.includes('feat')) kind = 'minor'
    else if (kinds.some((k) => ['fix', 'perf', 'refactor'].includes(k))) kind = 'patch'
    else kind = 'patch'
  }
  if (kind === 'major') return `${v.major + 1}.0.0`
  if (kind === 'minor') return `${v.major}.${v.minor + 1}.0`
  return `${v.major}.${v.minor}.${v.patch + 1}`
}

function releaseNotes(version, date, commits) {
  const grouped = new Map(GROUPS.map((g) => [g.key, []]))
  for (const c of commits) {
    const result = classify(c)
    const key = typeof result === 'string' ? result : result.key
    const text = typeof result === 'string' ? c.subject : result.text
    grouped.get(key).push(text)
  }
  const lines = [`## v${version}（${date}）`, '']
  let any = false
  for (const g of GROUPS) {
    const items = grouped.get(g.key)
    if (!items.length) continue
    any = true
    lines.push(`### ${g.title}`)
    for (const text of items.slice(0, 40)) lines.push(`- ${text}`)
    if (items.length > 40) lines.push(`- 另有 ${items.length - 40} 项改动，详见提交记录`)
    lines.push('')
  }
  if (!any) lines.push('- 常规维护与问题修复', '')
  return lines.join('\n').trimEnd() + '\n'
}

function updateChangelog(notes) {
  const path = resolve(ROOT, 'CHANGELOG.md')
  const old = existsSync(path) ? readFileSync(path, 'utf8') : '# 更新日志\n\n'
  const heading = '# 更新日志'
  const rest = old.startsWith(heading) ? old.slice(heading.length).replace(/^\s*\n/, '') : old
  writeFileSync(path, `${heading}\n\n${notes}\n\n${rest.trim() ? rest.trim() + '\n' : ''}`, 'utf8')
}

function writeIfChanged(path, content) {
  if (existsSync(path) && readFileSync(path, 'utf8') === content) return false
  writeFileSync(path, content, 'utf8')
  return true
}

function syncVersionFiles(version, date, notes) {
  const changed = []

  const configPath = resolve(ROOT, 'build/config.yml')
  const config = readFileSync(configPath, 'utf8')
  const nextConfig = config.replace(/(info:[\s\S]*?version:\s*)["']?[\d.]+["']?/, `$1"${version}"`)
  if (nextConfig !== config) {
    writeFileSync(configPath, nextConfig, 'utf8')
    changed.push('build/config.yml')
  }

  const infoPath = resolve(ROOT, 'build/windows/info.json')
  if (existsSync(infoPath)) {
    const info = readFileSync(infoPath, 'utf8')
    const next = info.replace(/"(file_version|product_version|FileVersion|ProductVersion)":\s*"[^"]*"/g, (_, key) => `"${key}": "${version}"`)
    if (next !== info) {
      writeFileSync(infoPath, next, 'utf8')
      changed.push('build/windows/info.json')
    }
  }

  for (const rel of ['build/windows/msix/app_manifest.xml', 'build/windows/msix/template.xml']) {
    const path = resolve(ROOT, rel)
    if (!existsSync(path)) continue
    const text = readFileSync(path, 'utf8')
    // 只改包版本：TargetDeviceFamily 的 MinVersion 是系统最低版本，不能被应用版本覆盖。
    const next = text.replace(
      /(<(?:Identity|PackageInformation)\b[^>]*?)Version="[\d.]+"/g,
      `$1Version="${version}.0"`,
    )
    if (next !== text) {
      writeFileSync(path, next, 'utf8')
      changed.push(rel)
    }
  }

  const manifest = {
    version,
    notes: notes.trim(),
    publishedAt: date,
    pageUrl: `${REPO_URL}/releases/tag/v${version}`,
    assets: {
      darwin: { name: 'flowlens.dmg', browser_download_url: `${REPO_URL}/releases/download/v${version}/flowlens.dmg`, size: 0 },
      windows: { name: 'flowlens-amd64-installer.exe', browser_download_url: `${REPO_URL}/releases/download/v${version}/flowlens-amd64-installer.exe`, size: 0 },
      linux: { name: 'flowlens-x86_64.AppImage', browser_download_url: `${REPO_URL}/releases/download/v${version}/flowlens-x86_64.AppImage`, size: 0 },
    },
  }
  if (writeIfChanged(resolve(ROOT, 'site/version.json'), JSON.stringify(manifest, null, 2) + '\n')) {
    changed.push('site/version.json')
  }
  return changed
}

function main() {
  const opts = args()
  const base = baseVersion(opts)
  const commits = commitsSince(base)
  const version = opts.version || opts.tag
    ? normalize(opts.version || opts.tag)
    : bumpVersion(base, commits, opts.bump)
  // 用本地日期（sv-SE 输出 YYYY-MM-DD），避免 UTC 与用户所在时区差一天。
  const date = new Date().toLocaleDateString('sv-SE')
  const notes = releaseNotes(version, date, commits)

  if (opts.dryRun) {
    process.stdout.write(`版本: ${base} -> ${version}\n提交: ${commits.length} 条\n\n${notes}`)
    return
  }

  updateChangelog(notes)
  writeFileSync(resolve(ROOT, 'RELEASE_NOTES.md'), notes, 'utf8')
  const changed = syncVersionFiles(version, date, notes)

  process.stdout.write(`version=${version}\n`)
  process.stdout.write(`notes=RELEASE_NOTES.md\n`)
  process.stdout.write(`变更文件: CHANGELOG.md, RELEASE_NOTES.md${changed.length ? ', ' + changed.join(', ') : ''}\n`)
  if (process.env.GITHUB_OUTPUT) {
    appendFileSync(process.env.GITHUB_OUTPUT, `version=${version}\n`)
  }
}

main()
