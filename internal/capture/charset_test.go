package capture

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
)

const sampleCN = "中文测试：响应体字符集探测正常，恭喜发财。"

// encodeTo 用指定编码把 UTF-8 字符串转成字节，用于构造测试数据。
func encodeTo(t *testing.T, enc encoding.Encoding, s string) []byte {
	t.Helper()
	out, err := enc.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	return out
}

func TestDecodeTextGBKByContentType(t *testing.T) {
	raw := encodeTo(t, simplifiedchinese.GBK, sampleCN)
	got, enc := DecodeText(raw, "text/html; charset=gbk")
	if got != sampleCN {
		t.Fatalf("GBK 解码结果不正确: %q", got)
	}
	if enc != "gb18030" {
		t.Fatalf("字符集标记应为 gb18030（GBK 的超集），实际 %s", enc)
	}
}

func TestDecodeTextGBKWithoutHint(t *testing.T) {
	// 没有任何 charset 线索时，应能靠候选探测识别出 GBK
	raw := encodeTo(t, simplifiedchinese.GBK, sampleCN)
	got, enc := DecodeText(raw, "text/plain")
	if got != sampleCN {
		t.Fatalf("无 charset 时 GBK 解码失败: %q", got)
	}
	if enc != "gb18030" {
		t.Fatalf("应识别为 gb18030，实际 %s", enc)
	}
}

func TestDecodeTextBig5(t *testing.T) {
	const sample = "繁體中文測試，編碼為 Big5。"
	raw := encodeTo(t, traditionalchinese.Big5, sample)
	got, enc := DecodeText(raw, `text/html; charset="big5"`)
	if got != sample {
		t.Fatalf("Big5 解码结果不正确: %q", got)
	}
	if enc != "big5" {
		t.Fatalf("应为 big5，实际 %s", enc)
	}
}

func TestDecodeTextUTF8BOM(t *testing.T) {
	raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte(sampleCN)...)
	got, enc := DecodeText(raw, "text/plain")
	if got != sampleCN {
		t.Fatalf("BOM 去除失败: %q", got)
	}
	if enc != "utf-8" {
		t.Fatalf("应为 utf-8，实际 %s", enc)
	}
}

func TestDecodeTextUTF16LE(t *testing.T) {
	out, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(sampleCN))
	if err != nil {
		t.Fatal(err)
	}
	got, enc := DecodeText(out, "text/plain")
	if got != sampleCN {
		t.Fatalf("UTF-16LE 解码失败: %q", got)
	}
	if enc != "utf-16le" {
		t.Fatalf("应为 utf-16le，实际 %s", enc)
	}
}

func TestDecodeTextMetaCharset(t *testing.T) {
	html := `<html><head><meta charset="gb2312"></head><body>` + sampleCN + `</body></html>`
	raw, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	got, enc := DecodeText(raw, "text/html") // 响应头没带 charset，只能靠 meta
	if !strings.Contains(got, sampleCN) {
		t.Fatalf("meta charset 未生效: %q", got)
	}
	if enc != "gb18030" {
		t.Fatalf("应为 gb18030，实际 %s", enc)
	}
}

func TestBuildBodyViewGBKJSON(t *testing.T) {
	body := `{"msg":"` + sampleCN + `"}`
	raw, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	v := BuildBodyView(raw, "application/json; charset=gbk", 1<<20)
	if v.Kind != "json" {
		t.Fatalf("应识别为 json，实际 %s", v.Kind)
	}
	if !strings.Contains(v.Preview, sampleCN) {
		t.Fatalf("GBK JSON 内容乱码: %q", v.Preview)
	}
	if v.Encoding != "gb18030" {
		t.Fatalf("encoding 字段应回传解码方式，实际 %q", v.Encoding)
	}
}

func TestBuildBodyViewBinaryNotDecoded(t *testing.T) {
	// PNG 头部 + 随机二进制，不能被当成文本渲染
	raw := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
	v := BuildBodyView(raw, "application/octet-stream", 1<<20)
	if v.Kind != "binary" {
		t.Fatalf("二进制内容应判为 binary，实际 %s（preview=%q）", v.Kind, v.Preview)
	}
}

func TestBuildBodyViewTruncated(t *testing.T) {
	raw := []byte(strings.Repeat("a", 1000))
	v := BuildBodyView(raw, "text/plain", 100)
	if !v.Truncated {
		t.Fatal("超过上限时 Truncated 应为 true")
	}
	if v.Size != 1000 {
		t.Fatalf("Size 应保留原始大小 1000，实际 %d", v.Size)
	}
	if len(v.Preview) != 100 {
		t.Fatalf("Preview 应截断到 100，实际 %d", len(v.Preview))
	}
}
