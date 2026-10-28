package geoip

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	"github.com/oschwald/geoip2-golang/v2"
)

func TestParseRegion(t *testing.T) {
	tests := []struct {
		in     string
		legacy bool
		want   Location
	}{
		{"中国|广东省|深圳市|移动|CN", false, Location{Country: "中国", CountryCode: "CN", Region: "广东省", City: "深圳市", ISP: "移动"}},
		{"United States|California|0|Google LLC|US", false, Location{Country: "United States", CountryCode: "US", Region: "California", ISP: "Google LLC"}},
		{"Reserved|Reserved|Reserved|0|0", false, Location{}},
		{"中国|0|江苏省|南京市|电信", true, Location{Country: "中国", CountryCode: "CN", Region: "江苏省", City: "南京市", ISP: "电信"}},
		{"美国|0|0|0|Level3", true, Location{Country: "美国", ISP: "Level3"}},
		{"0|0|0|内网IP|内网IP", true, Location{}},
		{"", false, Location{}},
	}
	for _, tt := range tests {
		if got := parseRegion(tt.in, tt.legacy); got != tt.want {
			t.Errorf("parseRegion(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestLocalName(t *testing.T) {
	n := geoip2.Names{English: "Beijing", SimplifiedChinese: "北京"}
	for lang, want := range map[string]string{"zh-CN": "北京", "en": "Beijing", "ja": "Beijing", "": "Beijing"} {
		if got := localName(n, lang); got != want {
			t.Errorf("localName(%q) = %q, want %q", lang, got, want)
		}
	}
}

func TestMapFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("hello xdb"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, unmap, err := mapFile(path)
	if err != nil || string(data) != "hello xdb" {
		t.Fatalf("mapFile = %q, %v", data, err)
	}
	if err := unmap(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenIP2RegionRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	// 文件头声明的段索引超出了文件长度，模拟下载不完整
	header := make([]byte, 1024)
	binary.LittleEndian.PutUint16(header[0:], xdb.Structure30)
	binary.LittleEndian.PutUint32(header[12:], 1<<20) // 最后一条段索引的位置
	binary.LittleEndian.PutUint16(header[16:], xdb.IPv4VersionNo)
	binary.LittleEndian.PutUint16(header[18:], 4)
	files := map[string][]byte{"truncated.xdb": header, "empty.xdb": nil, "garbage.xdb": []byte("not a xdb file")}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"truncated.xdb", "empty.xdb", "garbage.xdb", "missing.xdb"} {
		if _, err := OpenIP2Region(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

// 以下测试使用真实数据文件：把 GEOIP_TESTDATA 设为存放 mmdb / xdb 的目录，未设置时跳过

func testFile(tb testing.TB, name string) string {
	tb.Helper()
	path := filepath.Join(os.Getenv("GEOIP_TESTDATA"), name)
	if _, err := os.Stat(path); os.Getenv("GEOIP_TESTDATA") == "" || err != nil {
		tb.Skipf("GEOIP_TESTDATA 中没有 %s", name)
	}
	return path
}

func TestGeoLiteFile(t *testing.T) {
	g, err := OpenGeoLite(testFile(t, "GeoLite2-City.mmdb"), testFile(t, "GeoLite2-ASN.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if loc, err := g.Lookup(t.Context(), ip1, "zh-CN"); err != nil || loc.Country != "美国" || loc.CountryCode != "US" || loc.ASN != 15169 {
		t.Fatalf("Lookup = %+v, %v", loc, err)
	}
	if loc, _ := g.Lookup(t.Context(), ip1, "en"); loc.Country != "United States" {
		t.Fatalf("en: %+v", loc)
	}
	if loc, _ := g.Lookup(t.Context(), netip.MustParseAddr("1.1.1.1"), "zh-CN"); loc.CountryCode != "AU" {
		t.Fatalf("没有 country 时应使用注册国家: %+v", loc)
	}
	if _, err := g.Lookup(t.Context(), netip.MustParseAddr("192.168.1.1"), "zh-CN"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestIP2RegionFile(t *testing.T) {
	p, err := OpenIP2Region(testFile(t, "ip2region_v4.xdb"), testFile(t, "ip2region_v6.xdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for ip, want := range map[string]string{"223.5.5.5": "CN", "2400:3200::1": "CN", "8.8.8.8": "US", "::ffff:223.5.5.5": "CN"} {
		if loc, err := p.Lookup(t.Context(), netip.MustParseAddr(ip), ""); err != nil || loc.CountryCode != want {
			t.Errorf("Lookup(%s) = %+v, %v", ip, loc, err)
		}
	}
	if _, err := p.Lookup(t.Context(), netip.MustParseAddr("192.168.1.1"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := OpenIP2Region(testFile(t, "ip2region_v4.xdb"), testFile(t, "ip2region_v4.xdb")); err == nil {
		t.Fatal("重复的 IPv4 文件应报错")
	}
}

func BenchmarkLocalLookup(b *testing.B) {
	i2r, err := OpenIP2Region(testFile(b, "ip2region_v4.xdb"))
	if err != nil {
		b.Fatal(err)
	}
	geo, err := OpenGeoLite(testFile(b, "GeoLite2-City.mmdb"), "")
	if err != nil {
		b.Fatal(err)
	}
	defer geo.Close()
	r := NewResolver(Step{Provider: i2r, Country: "CN"}, Step{Provider: geo})
	ips := []netip.Addr{netip.MustParseAddr("223.5.5.5"), ip1}
	b.ReportAllocs()
	var i int
	for b.Loop() {
		r.Lookup(b.Context(), ips[i%len(ips)], "zh-CN")
		i++
	}
}
