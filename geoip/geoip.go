// Package geoip 聚合多个 IP 地理位置数据源：本地的 GeoLite2、ip2region，以及 ip-api.com、ipwho.is 等公共 API。
//
// Resolver 按查询链依次查询，数据源出错、限流、熔断或没有数据时自动降级到下一个；
// 远程数据源 Remote 自带结果缓存、同 IP 请求合并、令牌桶限流、429 退避与熔断，避免被上游拉黑。
package geoip

import (
	"context"
	"errors"
	"net/netip"
	"strings"
)

// Location 是统一的查询结果
type Location struct {
	IP          string  `json:"ip"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code,omitempty"` // ISO 3166-1 两位代码
	Region      string  `json:"region"`                 // 省 / 州
	City        string  `json:"city"`
	ISP         string  `json:"isp,omitempty"`
	ASN         uint    `json:"asn,omitempty"`
	Org         string  `json:"org,omitempty"` // 网络所属组织
	Latitude    float64 `json:"latitude,omitempty"`
	Longitude   float64 `json:"longitude,omitempty"`
	TimeZone    string  `json:"timezone,omitempty"`
	Bogon       bool    `json:"bogon,omitempty"`  // 内网、回环等保留地址，不会查询任何数据源
	Source      string  `json:"source,omitempty"` // 结果来自哪个数据源
}

// Provider 是一个 IP 数据源，实现需要并发安全
type Provider interface {
	Name() string
	// Lookup 查询 IP 信息，没有该 IP 的数据时返回 ErrNotFound。
	// 结果中的 IP 和 Source 由 Resolver 填充，数据源可以不填。
	Lookup(ctx context.Context, ip netip.Addr, lang string) (Location, error)
}

var (
	ErrNotFound      = errors.New("geoip: no data for this ip")
	ErrLimited       = errors.New("geoip: rate limited or paused")
	ErrUnknownSource = errors.New("geoip: unknown source")
)

// DefaultLang 是没有指定语言或语言无法识别时使用的语言
const DefaultLang = "zh-CN"

// NormalizeLang 从 lang 参数或 Accept-Language 中取第一个支持的语言：
// zh-CN、en、ja、de、fr、ru、es、pt-BR（GeoLite2、ip-api、ipwho.is 都支持这几种），都不支持时返回 ""。
func NormalizeLang(s string) string {
	for tag := range strings.SplitSeq(s, ",") {
		tag, _, _ = strings.Cut(tag, ";") // 去掉 q 权重
		primary, _, _ := strings.Cut(strings.ReplaceAll(strings.TrimSpace(tag), "_", "-"), "-")
		switch primary = strings.ToLower(primary); primary {
		case "zh":
			return "zh-CN"
		case "pt":
			return "pt-BR"
		case "en", "ja", "de", "fr", "ru", "es":
			return primary
		}
	}
	return ""
}

// bogonPrefixes 是 IsPrivate、IsGlobalUnicast 没有覆盖到的保留地址段
var bogonPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // 运营商级 NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),    // 文档
	netip.MustParsePrefix("198.18.0.0/15"),   // 基准测试
	netip.MustParsePrefix("198.51.100.0/24"), // 文档
	netip.MustParsePrefix("203.0.113.0/24"),  // 文档
	netip.MustParsePrefix("240.0.0.0/4"),     // 保留
	netip.MustParsePrefix("100::/64"),        // 黑洞
	netip.MustParsePrefix("2001:db8::/32"),   // 文档
}

// IsBogon 判断是否为内网、回环、链路本地、组播、文档用途等不会出现在公网上的地址
func IsBogon(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	for _, p := range bogonPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
