package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/time/rate"
)

func TestParseRate(t *testing.T) {
	tests := []struct {
		in    string
		limit rate.Limit
		burst int
		ok    bool
	}{
		{"40/1m", rate.Limit(40.0 / 60), 4, true},
		{"40/m", rate.Limit(40.0 / 60), 4, true},
		{"900/24h", rate.Limit(900.0 / 86400), 10, true},
		{"5/s", 5, 1, true},
		{"0", rate.Inf, 1, true},
		{"10", 0, 0, false},
		{"10/xyz", 0, 0, false},
		{"abc", 0, 0, false},
	}
	for _, tt := range tests {
		limit, burst, err := parseRate(tt.in)
		if (err == nil) != tt.ok || limit != tt.limit || burst != tt.burst {
			t.Errorf("parseRate(%q) = %v, %d, %v", tt.in, limit, burst, err)
		}
	}
}

func TestParsePrefixes(t *testing.T) {
	got, err := parsePrefixes("10.0.0.0/8, 192.168.1.1, ::1, none")
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.1.1/32"), netip.MustParsePrefix("::1/128")}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("parsePrefixes = %v, %v", got, err)
	}
	if _, err := parsePrefixes("bad"); err == nil {
		t.Fatal("非法地址应报错")
	}
}

func TestLoadConfig(t *testing.T) {
	for _, key := range []string{"GEOIP_LISTEN", "GEOIP_CHAIN", "GEOIP_LANG", "GEOIP_TRUSTED_PROXIES", "GEOIP_CACHE_TTL", "GEOIP_IPAPI_RATE"} {
		t.Setenv(key, "") // 空值表示使用默认值
	}
	c, err := loadConfig()
	if err != nil || c.listen != ":8080" || c.chain != "ipapi,ip2region@CN,geolite" || c.lang != "zh-CN" || len(c.trusted) != 2 {
		t.Fatalf("默认配置: %+v, %v", c, err)
	}
	t.Setenv("GEOIP_CACHE_TTL", "bad")
	t.Setenv("GEOIP_IPAPI_RATE", "fast")
	if _, err := loadConfig(); err == nil {
		t.Fatal("非法配置应报错")
	}
}

func TestBuildResolverSkipsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "GeoLite2-City.mmdb"), 0o755) // docker 挂载不存在的文件时会生成目录
	c := &config{
		chain:       "ipapi, ip2region@cn, geolite, ipwhois, ip2region",
		geoliteCity: filepath.Join(dir, "GeoLite2-City.mmdb"),
		ip2regionV4: filepath.Join(dir, "ip2region_v4.xdb"),
		ip2regionV6: filepath.Join(dir, "ip2region_v6.xdb"),
		cacheSize:   10,
	}
	r, cache, err := buildResolver(c)
	if err != nil || cache == nil || !slices.Equal(r.Chain(), []string{"ipapi", "ipwhois"}) {
		t.Fatalf("chain = %v, %v", r.Chain(), err)
	}
	c.chain = "geolite"
	if _, _, err := buildResolver(c); err == nil {
		t.Fatal("没有可用数据源时应报错")
	}
	c.chain = "ipapi,unknown"
	if _, _, err := buildResolver(c); err == nil {
		t.Fatal("未知数据源应报错")
	}
}

func TestBuildResolverWithData(t *testing.T) {
	dir := os.Getenv("GEOIP_TESTDATA")
	if dir == "" {
		t.Skip("未设置 GEOIP_TESTDATA")
	}
	c := &config{
		chain:       "ip2region@CN,geolite",
		geoliteCity: filepath.Join(dir, "GeoLite2-City.mmdb"),
		geoliteASN:  filepath.Join(dir, "GeoLite2-ASN.mmdb"),
		ip2regionV4: filepath.Join(dir, "ip2region_v4.xdb"),
		ip2regionV6: filepath.Join(dir, "ip2region_v6.xdb"),
	}
	r, _, err := buildResolver(c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if loc, err := r.Lookup(t.Context(), netip.MustParseAddr("120.229.10.10"), ""); err != nil || loc.Source != "ip2region" || loc.ISP == "" {
		t.Fatalf("国内 IP: %+v, %v", loc, err)
	}
	if loc, err := r.Lookup(t.Context(), netip.MustParseAddr("8.8.8.8"), ""); err != nil || loc.Source != "geolite" || loc.Country != "美国" {
		t.Fatalf("海外 IP: %+v, %v", loc, err)
	}
}
