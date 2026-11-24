package proxy

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/kerison/flowlens/internal/ca"
	"github.com/kerison/flowlens/internal/capture"
	"github.com/kerison/flowlens/internal/config"
)

// 这些用例只覆盖"错误怎么翻译成人话"这一层：
// 真正的自检要走网络与系统信任库，不适合放进单元测试。

func TestClassifyUnknownAuthority(t *testing.T) {
	res := SelfTestResult{}
	classifySelfTestError(&res, x509.UnknownAuthorityError{})

	if !strings.Contains(res.Message, "证书未被信任") {
		t.Fatalf("应识别为证书未信任，实际：%s", res.Message)
	}
	if !strings.Contains(res.Hint, "安装") {
		t.Fatalf("应引导去安装证书，实际：%s", res.Hint)
	}
}

func TestClassifyCertificateInvalid(t *testing.T) {
	res := SelfTestResult{}
	classifySelfTestError(&res, x509.CertificateInvalidError{Reason: x509.Expired})

	if !strings.Contains(res.Message, "证书校验失败") {
		t.Fatalf("应识别为证书校验失败，实际：%s", res.Message)
	}
	if !strings.Contains(res.Hint, "重新安装") {
		t.Fatalf("应提示重新安装，实际：%s", res.Hint)
	}
}

func TestClassifyHostnameMismatch(t *testing.T) {
	res := SelfTestResult{}
	classifySelfTestError(&res, x509.HostnameError{
		Certificate: &x509.Certificate{Subject: pkix.Name{CommonName: "a"}},
		Host:        "b",
	})
	if !strings.Contains(res.Message, "域名不匹配") {
		t.Fatalf("应识别为域名不匹配，实际：%s", res.Message)
	}
}

func TestClassifyConnRefused(t *testing.T) {
	res := SelfTestResult{TargetURL: "https://example.com/"}
	err := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	classifySelfTestError(&res, err)

	if !strings.Contains(res.Message, "连不上本机代理") {
		t.Fatalf("应识别为连不上代理，实际：%s", res.Message)
	}
}

func TestClassifyDNS(t *testing.T) {
	res := SelfTestResult{}
	classifySelfTestError(&res, &net.DNSError{Err: "no such host", Name: "x.invalid"})

	if !strings.Contains(res.Message, "外网不可达") {
		t.Fatalf("应识别为外网不可达，实际：%s", res.Message)
	}
	if res.Hint == "" {
		t.Fatal("DNS 失败也应给出建议")
	}
}

func TestClassifyTimeout(t *testing.T) {
	res := SelfTestResult{}
	classifySelfTestError(&res, os.ErrDeadlineExceeded)

	if !strings.Contains(res.Message, "外网不可达") {
		t.Fatalf("超时应归入外网不可达，实际：%s", res.Message)
	}
}

func TestSelfTestRequiresRunningProxy(t *testing.T) {
	cfg, err := config.NewManagerAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caMgr, err := ca.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, caMgr, capture.NewStore(10), nil)

	res := m.SelfTest("")
	if res.OK {
		t.Fatal("代理未启动时自检不应通过")
	}
	if res.TargetURL != DefaultSelfTestTarget {
		t.Fatalf("空目标应回落到默认站点，实际：%s", res.TargetURL)
	}
	if !strings.Contains(res.Message, "未启动") {
		t.Fatalf("应提示代理未启动，实际：%s", res.Message)
	}
	if res.Hint == "" {
		t.Fatal("应给出下一步建议")
	}
}
