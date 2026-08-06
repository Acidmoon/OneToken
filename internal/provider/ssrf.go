// SSRF 防护（设计 §6.4、§10.1-5）：
//   - 禁用重定向（CheckRedirect，provider.go 已配置）；
//   - scheme 校验（validateBaseURL，https 且 localhost 例外）；
//   - 内网/私有段拦截（环回放行——本地部署通道合法；RFC1918/链路本地/
//     CGNAT/IPv6 ULA/多播/未指定 拦截；可配置白名单 ssrf_allow）；
//   - DNS rebinding：DialContext 中 解析→校验→拨号（用解析出的 IP 拨号，
//     防解析侧 TOCTOU 绕过）。
//
// 局限：经系统代理（HTTP_PROXY 等）的请求由代理转发，不经过本 DialContext
// 校验——代理是用户显式信任的（ProxyFromEnvironment 默认保留）。
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ErrSSRFBlocked 目标地址被 SSRF 拦截（内网/私有段）。
var ErrSSRFBlocked = errors.New("provider: SSRF 拦截（目标为内网/私有地址）")

// lookupIP 解析主机名为 IP 列表。包级可注入，便于 DNS rebinding 单测。
var lookupIP = net.DefaultResolver.LookupIP

// isBlockedIP 判定地址是否属于拦截段（设计 §6.4 清单 + 安全补全）。
// 环回（127/8、::1）放行：本地部署通道（vLLM/Ollama）是设计认可的合法场景，
// 与 scheme 校验的 localhost 例外一致。
// 拦截：RFC1918（10/8、172.16/12、192.168/16）、IPv4 链路本地 169.254/16、
// IPv6 链路本地 fe80::/10、CGNAT 100.64/10、IPv6 ULA fc00::/7、
// 多播（224/4、ff00::/8）、未指定（0.0.0.0、::）。
func isBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap() // IPv4-mapped IPv6（::ffff:a.b.c.d）解包后按 IPv4 判定
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if ip.Is4() {
		a := ip.As4()
		// CGNAT 100.64.0.0/10（netip 无专用方法）
		if a[0] == 100 && a[1] >= 64 && a[1] <= 127 {
			return true
		}
		// 受限广播与保留段补全（审查 S-L1）：255.255.255.255（netip 的
		// IsMulticast 不含 255）、Class E 240/4、IETF 协议保留 192.0.0.0/24、
		// 6to4 relay anycast 192.88.99.0/24、基准测试段 198.18.0.0/15
		switch {
		case a[0] == 255 || a[0] >= 240:
			return true
		case a[0] == 192 && a[1] == 0 && a[2] == 0:
			return true // 192.0.0.0/24
		case a[0] == 192 && a[1] == 88 && a[2] == 99:
			return true // 192.88.99.0/24
		case a[0] == 198 && (a[1] == 18 || a[1] == 19):
			return true // 198.18.0.0/15
		}
	}
	if ip.Is6() {
		b := ip.As16()
		// fec0::/10（RFC 3879 已废弃的 site-local，遗留网络可能可达）
		if b[0] == 0xfe && b[1]&0xc0 == 0xc0 {
			return true
		}
	}
	return false
}

// parseAllowList 解析 SSRF 白名单（IP 或 CIDR，如 "127.0.0.0/8"、"10.0.0.1"）。
func parseAllowList(allow []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(allow))
	for _, s := range allow {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		return nil, fmt.Errorf("provider: ssrf_allow 非法条目 %q（期望 IP 或 CIDR）", s)
	}
	return out, nil
}

// isAllowed 判定地址是否允许拨号：环回例外放行 → 白名单命中 → 其余按拦截段判定
// （公网不拦截）。
func isAllowed(ip netip.Addr, allow []netip.Prefix) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true // 本地通道例外
	}
	for _, p := range allow {
		if p.Contains(ip) {
			return true
		}
	}
	return !isBlockedIP(ip)
}

// secureDialContext 构造带 SSRF 校验的拨号函数（解析→校验→拨号）。
// 解析出的全部地址逐一校验；任一命中拦截即拒绝（不拨号）；
// 全部通过后用解析出的 IP 拨号（而非域名），消除 解析→拨号 窗口的
// DNS rebinding 绕过；TLS ServerName 仍取 URL 主机名（http.Transport 行为）。
func secureDialContext(allow []netip.Prefix, dialTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("provider: 拨号地址非法 %q: %w", addr, err)
		}
		ips, err := lookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("provider: 解析 %s: %w", host, err)
		}
		var dialAddrs []string
		for _, ip := range ips {
			a, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			if !isAllowed(a, allow) {
				return nil, fmt.Errorf("%w: %s 解析到 %s", ErrSSRFBlocked, host, ip)
			}
			dialAddrs = append(dialAddrs, net.JoinHostPort(ip.String(), port))
		}
		if len(dialAddrs) == 0 {
			return nil, fmt.Errorf("provider: %s 无可拨号地址", host)
		}
		var lastErr error
		for _, d := range dialAddrs {
			conn, err := dialer.DialContext(ctx, network, d)
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}
