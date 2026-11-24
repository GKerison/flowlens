package capture

import (
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
)

// maxBadRatio 是可接受的最大"解码失败"字符占比。
// 1% 的依据：真实文本里几乎不可能出现替换符，一旦出现基本就是选错了编码；
// 而最后被截断的那个多字节字符最多贡献 1 个坏字符，短文本时也不会误判。
const maxBadRatio = 0.01

// sniffLen 是 meta charset 的嗅探范围。HTML 规范建议读前 1024 字节，这里保持一致。
const sniffLen = 1024

// autoCandidates 是无任何 charset 线索时的猜测顺序。
//
// 为什么按顺序"先到先得"而不是全局打分择优：GBK 与 Big5 的字节区间大量重叠，
// 用 Big5 解 GBK 文本同样能产出"合法"的汉字（只是全是错的），打分无法区分。
// 按目标用户分布排序，先命中谁就用谁，比打分更可靠也更可预测。
var autoCandidates = []string{"gb18030", "big5", "shift_jis", "euc-kr"}

// DecodeText 把原始字节解码为字符串，并返回实际使用的字符集名（小写规范名）。
//
// 决策顺序（短路返回）：
//  1. BOM —— 最可靠的信号，直接决定 UTF-8/UTF-16；
//  2. Content-Type 里的 charset —— 服务端显式声明的优先；
//  3. HTML <meta charset> —— 老站点常把编码写在页面里而不是响应头；
//  4. 内容本身是合法 UTF-8 —— 无需转换；
//  5. 候选编码逐个尝试，取第一个"解码质量达标"的；
//  6. 全部失败 —— 保留原字节并替换非法序列，字符集标记为 unknown。
func DecodeText(raw []byte, contentType string) (text, enc string) {
	if len(raw) == 0 {
		return "", ""
	}

	if s, name, ok := decodeBOM(raw); ok {
		return s, name
	}

	if name := charsetFromContentType(contentType); name != "" {
		if s, ok := decodeByName(name, raw); ok {
			return s, canonicalName(name)
		}
	}

	if name := sniffMetaCharset(raw); name != "" {
		if s, ok := decodeByName(name, raw); ok {
			return s, canonicalName(name)
		}
	}

	if utf8.Valid(raw) {
		return string(raw), "utf-8"
	}

	for _, name := range autoCandidates {
		if s, ok := decodeByName(name, raw); ok {
			return s, canonicalName(name)
		}
	}

	return strings.ToValidUTF8(string(raw), "\uFFFD"), "unknown"
}

// decodeBOM 处理带字节序标记的内容。
func decodeBOM(raw []byte) (string, string, bool) {
	switch {
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		return string(raw[3:]), "utf-8", true
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		s, ok := decodeEnc(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), raw[2:])
		return s, "utf-16le", ok
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		s, ok := decodeEnc(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), raw[2:])
		return s, "utf-16be", ok
	case len(raw) >= 4 && raw[0] == 0 && raw[1] == 0 && raw[2] == 0xFE && raw[3] == 0xFF:
		// UTF-32 在 HTTP 里极罕见，识别到也只是为了不把它当二进制
		return "", "utf-32", false
	}
	return "", "", false
}

// charsetFromContentType 从 Content-Type 中取出 charset 参数。
func charsetFromContentType(contentType string) string {
	if contentType == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Content-Type 不规范时退化为手工查找，避免因为一个多余分号就失去编码线索
		lower := strings.ToLower(contentType)
		if i := strings.Index(lower, "charset="); i >= 0 {
			v := lower[i+len("charset="):]
			if j := strings.IndexAny(v, ";, \t"); j >= 0 {
				v = v[:j]
			}
			return strings.Trim(v, `"'`)
		}
		return ""
	}
	return params["charset"]
}

// sniffMetaCharset 在 HTML 头部查找 <meta charset="..."> 或
// <meta http-equiv="Content-Type" content="text/html; charset=...">。
func sniffMetaCharset(raw []byte) string {
	head := raw
	if len(head) > sniffLen {
		head = head[:sniffLen]
	}
	lower := strings.ToLower(string(head))
	if i := strings.Index(lower, "<meta"); i < 0 {
		return ""
	}
	// 直接复用"找 charset=" 的逻辑：meta 的两种写法里都有这个片段
	if i := strings.Index(lower, "charset="); i >= 0 {
		v := lower[i+len("charset="):]
		v = strings.TrimLeft(v, `"' `)
		if j := strings.IndexAny(v, `"'; >/`); j >= 0 {
			v = v[:j]
		}
		v = strings.TrimSpace(v)
		if v != "" && v != "utf-8" && v != "utf8" {
			return v
		}
		if v != "" {
			return "utf-8"
		}
	}
	return ""
}

// decodeByName 按名称解码并检查质量。
func decodeByName(name string, raw []byte) (string, bool) {
	enc, err := htmlindex.Get(name)
	if err != nil || enc == nil {
		return "", false
	}
	return decodeEnc(enc, raw)
}

// decodeEnc 执行解码；只有"坏字符占比达标"才认为猜测成立。
func decodeEnc(enc encoding.Encoding, raw []byte) (string, bool) {
	if enc == nil {
		return "", false
	}
	out, err := enc.NewDecoder().Bytes(raw)
	// 内容被截断时尾部会报 ErrShortSrc，但已解码部分是可用的，因此不看 err
	if err != nil && len(out) == 0 {
		return "", false
	}
	s := string(out)
	if badRatio(s) > maxBadRatio {
		return "", false
	}
	return s, true
}

// badRatio 统计"解码明显出错"的字符占比：
// 替换符 U+FFFD、C0/C1 控制符、私用区字符（GBK/Big5 乱码的典型产物）。
func badRatio(s string) float64 {
	if s == "" {
		return 1 // 解出来是空字符串说明没解出任何东西，等同于失败
	}
	var bad, total int
	for _, r := range s {
		total++
		switch {
		case r == utf8.RuneError:
			bad++
		case r < 0x20 && r != '\t' && r != '\n' && r != '\r':
			bad++
		case r == 0x7F || (r >= 0x80 && r <= 0x9F):
			bad++
		case r >= 0xE000 && r <= 0xF8FF:
			bad++
		}
	}
	return float64(bad) / float64(total)
}

// canonicalName 把别名归一成简短可读的名字，用于界面展示。
func canonicalName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "utf8", "utf-8":
		return "utf-8"
	case "gb2312", "gbk", "gb18030", "cp936", "x-gbk":
		// 中文站点常声明 gb2312 但实际用 GBK/GB18030 字符，统一标成 gb18030 更准确
		return "gb18030"
	case "big5", "big-5", "cp950":
		return "big5"
	case "utf16", "utf-16", "utf-16le":
		return "utf-16le"
	case "utf-16be":
		return "utf-16be"
	}
	if e, err := htmlindex.Get(name); err == nil && e != nil {
		if n, err := htmlindex.Name(e); err == nil && n != "" {
			return strings.ToLower(n)
		}
	}
	return name
}

// looksLikeText 判断未知 MIME 的内容是否应按文本处理。
// 判定标准：不含 NUL 字节，且要么是合法 UTF-8，要么能被某个候选编码干净解出。
func looksLikeText(raw []byte) bool {
	probe := raw
	if len(probe) > 512 {
		probe = probe[:512]
	}
	if len(probe) == 0 {
		return true
	}
	for _, b := range probe {
		if b == 0 {
			return false
		}
	}
	if utf8.Valid(probe) {
		return true
	}
	for _, name := range autoCandidates {
		if _, ok := decodeByName(name, probe); ok {
			return true
		}
	}
	return false
}

// isTextualMIME 判断 Content-Type 是否本身就声明为文本类。
func isTextualMIME(mimeType string) bool {
	return isText(mimeType) ||
		isJSON(mimeType) ||
		strings.Contains(mimeType, "graphql") ||
		strings.Contains(mimeType, "x-ndjson")
}
