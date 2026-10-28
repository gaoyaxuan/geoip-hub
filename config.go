package main

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gaoyaxuan/geoip-hub/geoip"
	"golang.org/x/time/rate"
)

// config 来自环境变量，说明见 README
type config struct {
	listen   string
	chain    string
	lang     string
	batchMax int
	trusted  []netip.Prefix

	geoliteCity, geoliteASN  string
	ip2regionV4, ip2regionV6 string

	cacheSize         int
	cacheTTL, missTTL time.Duration
	timeout           time.Duration
	ipapi, ipwhois    remoteConfig
}

type remoteConfig struct {
	key   string
	limit rate.Limit
	burst int
}

func loadConfig() (*config, error) {
	e := &envReader{}
	c := &config{
		listen:      e.str("GEOIP_LISTEN", ":8080"),
		chain:       e.str("GEOIP_CHAIN", "ipapi,ip2region@CN,geolite"),
		lang:        cmp.Or(geoip.NormalizeLang(e.str("GEOIP_LANG", "")), geoip.DefaultLang),
		batchMax:    e.num("GEOIP_BATCH_MAX", 100),
		trusted:     e.prefixes("GEOIP_TRUSTED_PROXIES", "0.0.0.0/0,::/0"),
		geoliteCity: e.str("GEOIP_GEOLITE_CITY", "GeoLite2-City.mmdb"),
		geoliteASN:  e.str("GEOIP_GEOLITE_ASN", "GeoLite2-ASN.mmdb"),
		ip2regionV4: e.str("GEOIP_IP2REGION_V4", "ip2region_v4.xdb"),
		ip2regionV6: e.str("GEOIP_IP2REGION_V6", "ip2region_v6.xdb"),
		cacheSize:   e.num("GEOIP_CACHE_SIZE", 100_000),
		cacheTTL:    e.dur("GEOIP_CACHE_TTL", 24*time.Hour),
		missTTL:     e.dur("GEOIP_CACHE_MISS_TTL", 10*time.Minute),
		timeout:     e.dur("GEOIP_TIMEOUT", 3*time.Second),
		ipapi:       e.remote("GEOIP_IPAPI", "40/1m"),
		ipwhois:     e.remote("GEOIP_IPWHOIS", "900/24h"),
	}
	return c, errors.Join(e.errs...)
}

// errNoData 表示本地数据文件不存在，该数据源会被跳过
var errNoData = errors.New("数据文件不存在")

// buildResolver 按 GEOIP_CHAIN 创建数据源并组装查询链，数据文件不存在的本地数据源会被跳过
func buildResolver(c *config) (*geoip.Resolver, *geoip.Cache, error) {
	cache := geoip.NewCache(c.cacheSize, c.cacheTTL, c.missTTL)
	opened := map[string]geoip.Provider{}
	var steps []geoip.Step
	for item := range strings.SplitSeq(c.chain, ",") {
		name, country, _ := strings.Cut(strings.TrimSpace(item), "@")
		if name == "" {
			continue
		}
		p, ok := opened[name]
		if !ok {
			var err error
			if p, err = c.open(name, cache); errors.Is(err, errNoData) {
				slog.Warn("跳过数据源", "provider", name, "reason", err)
			} else if err != nil {
				return nil, nil, err
			}
			opened[name] = p
		}
		if p != nil {
			steps = append(steps, geoip.Step{Provider: p, Country: strings.ToUpper(country)})
		}
	}
	if len(steps) == 0 {
		return nil, nil, errors.New("没有可用的数据源，请检查 GEOIP_CHAIN 和数据文件")
	}
	return geoip.NewResolver(steps...), cache, nil
}

// open 按名称创建数据源
func (c *config) open(name string, cache *geoip.Cache) (geoip.Provider, error) {
	remote := func(api geoip.API, rc remoteConfig) geoip.Provider {
		return geoip.NewRemote(api, geoip.RemoteOptions{
			Key: rc.key, Limit: rc.limit, Burst: rc.burst, Timeout: c.timeout, Cache: cache,
		})
	}
	switch name {
	case "ipapi":
		return remote(geoip.IPAPI, c.ipapi), nil
	case "ipwhois":
		return remote(geoip.IPWhois, c.ipwhois), nil
	case "geolite":
		if !isFile(c.geoliteCity) {
			return nil, fmt.Errorf("%w: %s", errNoData, c.geoliteCity)
		}
		asn := c.geoliteASN
		if !isFile(asn) {
			asn = "" // ASN 库是可选的
		}
		g, err := geoip.OpenGeoLite(c.geoliteCity, asn)
		if err != nil {
			return nil, fmt.Errorf("geolite: %w", err)
		}
		slog.Info("数据源已加载", "provider", name, "city", c.geoliteCity, "asn", cmp.Or(asn, "无"),
			"built", g.BuildTime().Format(time.DateOnly))
		return g, nil
	case "ip2region":
		files := slices.DeleteFunc([]string{c.ip2regionV4, c.ip2regionV6}, func(f string) bool { return !isFile(f) })
		if len(files) == 0 {
			return nil, fmt.Errorf("%w: %s, %s", errNoData, c.ip2regionV4, c.ip2regionV6)
		}
		p, err := geoip.OpenIP2Region(files...)
		if err != nil {
			return nil, fmt.Errorf("ip2region: %w", err)
		}
		slog.Info("数据源已加载", "provider", name, "files", strings.Join(files, ", "),
			"built", p.BuildTime().Format(time.DateOnly))
		return p, nil
	}
	return nil, fmt.Errorf("GEOIP_CHAIN 中有未知的数据源 %q，可选：ipapi、ipwhois、geolite、ip2region", name)
}

// isFile 判断路径是否为普通文件（docker 挂载不存在的文件时会生成同名目录）
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// parseRate 解析 "40/1m"、"900/24h"、"5/s" 形式的限速，"0" 表示不限速；突发容量取次数的 1/10（1~10）
func parseRate(s string) (rate.Limit, int, error) {
	n, per, _ := strings.Cut(s, "/")
	count, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil {
		return 0, 0, fmt.Errorf("无效的限速 %q，格式如 40/1m", s)
	}
	if count <= 0 {
		return rate.Inf, 1, nil
	}
	per = strings.TrimSpace(per)
	if per != "" && (per[0] < '0' || per[0] > '9') {
		per = "1" + per // 40/m 等同于 40/1m
	}
	d, err := time.ParseDuration(per)
	if err != nil || d <= 0 {
		return 0, 0, fmt.Errorf("无效的限速 %q，格式如 40/1m", s)
	}
	return rate.Limit(float64(count) / d.Seconds()), min(max(count/10, 1), 10), nil
}

// parsePrefixes 解析逗号分隔的 CIDR 或 IP 列表，"none" 表示空列表
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var list []netip.Prefix
	for item := range strings.SplitSeq(s, ",") {
		switch item = strings.TrimSpace(item); {
		case item == "" || item == "none":
		case strings.Contains(item, "/"):
			p, err := netip.ParsePrefix(item)
			if err != nil {
				return nil, err
			}
			list = append(list, p.Masked())
		default:
			ip, err := netip.ParseAddr(item)
			if err != nil {
				return nil, err
			}
			list = append(list, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
		}
	}
	return list, nil
}

// envReader 读取环境变量，未设置时使用默认值，并收集格式错误
type envReader struct{ errs []error }

func (e *envReader) str(key, def string) string {
	return cmp.Or(strings.TrimSpace(os.Getenv(key)), def)
}

func (e *envReader) num(key string, def int) int {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: 无效的数字 %q", key, v))
		return def
	}
	return n
}

func (e *envReader) dur(key string, def time.Duration) time.Duration {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: 无效的时长 %q，格式如 30s、10m、24h", key, v))
		return def
	}
	return d
}

func (e *envReader) prefixes(key, def string) []netip.Prefix {
	list, err := parsePrefixes(e.str(key, def))
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
	}
	return list
}

func (e *envReader) remote(prefix, defRate string) remoteConfig {
	c := remoteConfig{key: e.str(prefix+"_KEY", "")}
	var err error
	if c.limit, c.burst, err = parseRate(e.str(prefix+"_RATE", defRate)); err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s_RATE: %w", prefix, err))
	}
	return c
}
