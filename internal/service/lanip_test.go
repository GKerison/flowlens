package service

import (
	"net"
	"testing"
)

// 手机接入向导里只应出现"手机真能连到"的地址。
// 用例覆盖：网卡名识别（大小写/localized）、固定虚拟网段兜底、真实地址保留。

func TestIsVirtualInterface(t *testing.T) {
	virtual := []string{
		"vmnet8", "VirtualBox Host-Only Ethernet Adapter", "VMware Virtual Ethernet Adapter",
		"Parallels Host-Guest Networking Adapter", "vEthernet (Default Switch)",
		"utun4", "docker0", "tailscale0", "awdl0",
	}
	for _, name := range virtual {
		if !isVirtualInterface(name) {
			t.Errorf("%q 应识别为虚拟网卡", name)
		}
	}

	real := []string{"en0", "en1", "eth0", "wlan0", "Wi-Fi"}
	for _, name := range real {
		if isVirtualInterface(name) {
			t.Errorf("%q 是真实网卡，不应被过滤", name)
		}
	}
}

func TestIsVirtualAddress(t *testing.T) {
	virtual := []string{"10.211.55.2", "10.37.129.3", "192.168.56.1"}
	for _, s := range virtual {
		if !isVirtualAddress(net.ParseIP(s)) {
			t.Errorf("%s 应识别为虚拟机网段", s)
		}
	}

	real := []string{"192.168.1.23", "10.0.0.5", "172.16.3.9", "10.211.56.2"}
	for _, s := range real {
		if isVirtualAddress(net.ParseIP(s)) {
			t.Errorf("%s 是常见真实局域网地址，不应被过滤", s)
		}
	}
}
