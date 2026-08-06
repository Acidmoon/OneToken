package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		// RFC1918
		{"10/8", "10.0.0.1", true},
		{"172.16/12 起点", "172.16.0.1", true},
		{"172.31/12 终点", "172.31.255.255", true},
		{"172.32 界外", "172.32.0.1", false},
		{"192.168/16", "192.168.1.1", true},
		// 链路本地
		{"169.254/16（云元数据）", "169.254.169.254", true},
		{"fe80::/10", "fe80::1", true},
		// CGNAT
		{"100.64/10 起点", "100.64.0.1", true},
		{"100.127 终点", "100.127.255.255", true},
		{"100.128 界外", "100.128.0.1", false},
		// 环回（拦截段清单内；放行逻辑见 isAllowed）
		{"127.0.0.1", "127.0.0.1", true},
		{"::1", "::1", true},
		// IPv6 ULA / 多播 / 未指定
		{"fc00::/7", "fc00::1", true},
		{"fd00::/7", "fd00::1234", true},
		{"ff02::1 多播", "ff02::1", true},
		{"224.0.0.1 多播", "224.0.0.1", true},
		{"0.0.0.0 未指定", "0.0.0.0", true},
		{":: 未指定", "::", true},
		// 保留段补全（审查 S-L1）
		{"255.255.255.255 受限广播", "255.255.255.255", true},
		{"240/4 Class E", "240.0.0.1", true},
		{"239.255.255.255 多播上界", "239.255.255.255", true},
		{"fec0::/10 废弃 site-local", "fec0::1", true},
		{"192.0.0.0/24", "192.0.0.8", true},
		{"192.88.99.0/24", "192.88.99.1", true},
		{"198.18.0.0/15", "198.18.0.1", true},
		{"198.19.255.255", "198.19.255.255", true},
		{"198.20.0.1 界外", "198.20.0.1", false},
		// IPv4-mapped IPv6
		{"::ffff:10.0.0.1", "::ffff:10.0.0.1", true},
		{"::ffff:192.168.0.1", "::ffff:192.168.0.1", true},
		// 公网放行
		{"8.8.8.8", "8.8.8.8", false},
		{"1.1.1.1", "1.1.1.1", false},
		{"2606:4700::1111", "2606:4700::1111", false},
		{"::ffff:8.8.8.8", "::ffff:8.8.8.8", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := netip.MustParseAddr(c.ip)
			if got := isBlockedIP(a); got != c.want {
				t.Fatalf("isBlockedIP(%s)=%v，期望 %v", c.ip, got, c.want)
			}
		})
	}
}

func TestIsAllowedLoopbackAndWhitelist(t *testing.T) {
	allow, err := parseAllowList([]string{"172.16.0.0/12", "10.5.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},  // 环回例外
		{"::1", true},        // 环回例外
		{"172.17.0.2", true}, // 白名单 CIDR
		{"10.5.0.1", true},   // 白名单精确 IP
		{"192.168.1.1", false},
		{"8.8.8.8", true}, // 公网本就放行
	}
	for _, c := range cases {
		a := netip.MustParseAddr(c.ip)
		if got := isAllowed(a, allow); got != c.want {
			t.Errorf("isAllowed(%s)=%v，期望 %v", c.ip, got, c.want)
		}
	}
}

func TestParseAllowList(t *testing.T) {
	if _, err := parseAllowList([]string{"10.0.0.0/8", "1.2.3.4"}); err != nil {
		t.Fatalf("合法白名单应通过: %v", err)
	}
	if _, err := parseAllowList([]string{"not-an-ip"}); err == nil {
		t.Fatal("非法条目应报错")
	}
	if _, err := parseAllowList([]string{""}); err != nil {
		t.Fatalf("空条目应跳过: %v", err)
	}
}

// TestSecureDialDNSRebinding 验证 DNS rebinding 防护：域名解析出内网 IP → 拨号前拦截。
func TestSecureDialDNSRebinding(t *testing.T) {
	old := lookupIP
	lookupIP = func(_ context.Context, _, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.1")}, nil
	}
	defer func() { lookupIP = old }()

	dial := secureDialContext(nil, time.Second)
	_, err := dial(context.Background(), "tcp", "evil.example.com:443")
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("期望 ErrSSRFBlocked，实际 %v", err)
	}
}

// TestSecureDialBlockedAny 验证多 IP 中任一命中即拦截（解析到混合地址）。
func TestSecureDialBlockedAny(t *testing.T) {
	old := lookupIP
	lookupIP = func(_ context.Context, _, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("169.254.169.254")}, nil
	}
	defer func() { lookupIP = old }()

	dial := secureDialContext(nil, time.Second)
	_, err := dial(context.Background(), "tcp", "both.example.com:443")
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("任一内网 IP 即应拦截，实际 %v", err)
	}
}

// TestSecureDialLoopback 验证环回放行 + 真实拨号（本地部署通道合法，httptest 即 127.0.0.1）。
func TestSecureDialLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	dial := secureDialContext(nil, 2*time.Second)
	conn, err := dial(context.Background(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("环回应放行可拨号: %v", err)
	}
	conn.Close()
}

// TestSecureDialWhitelist 验证白名单放行内网地址（不返回 ErrSSRFBlocked，改为连接失败）。
func TestSecureDialWhitelist(t *testing.T) {
	old := lookupIP
	lookupIP = func(_ context.Context, _, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("172.17.0.2")}, nil
	}
	defer func() { lookupIP = old }()

	allow, err := parseAllowList([]string{"172.16.0.0/12"})
	if err != nil {
		t.Fatal(err)
	}
	dial := secureDialContext(allow, 50*time.Millisecond)
	_, err = dial(context.Background(), "tcp", "docker.example.com:81")
	if errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("白名单内地址不应被 SSRF 拦截")
	}
	if err == nil {
		t.Fatalf("应因不可达而连接失败（测试预期）")
	}
}

func TestSecureDialNoAddr(t *testing.T) {
	old := lookupIP
	lookupIP = func(_ context.Context, _, _ string) ([]net.IP, error) {
		return nil, nil
	}
	defer func() { lookupIP = old }()

	dial := secureDialContext(nil, time.Second)
	if _, err := dial(context.Background(), "tcp", "empty.example.com:80"); err == nil {
		t.Fatal("解析为空应报错")
	}
}
