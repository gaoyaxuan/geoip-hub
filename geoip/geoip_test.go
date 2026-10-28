package geoip

import (
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

func TestNormalizeLang(t *testing.T) {
	tests := map[string]string{
		"":                        "",
		"zh-CN,zh;q=0.9,en;q=0.8": "zh-CN",
		"zh-TW":                   "zh-CN",
		"zh_CN":                   "zh-CN",
		"en-US,en;q=0.9":          "en",
		"pt-PT":                   "pt-BR",
		"ko-KR, ja;q=0.8":         "ja",
		"ko":                      "",
		"*":                       "",
	}
	for in, want := range tests {
		if got := NormalizeLang(in); got != want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsBogon(t *testing.T) {
	for _, s := range []string{
		"0.0.0.0", "0.1.2.3", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.1.1", "172.16.0.1",
		"192.168.1.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fe80::1", "fc00::1", "ff02::1", "2001:db8::1", "::ffff:192.168.1.1",
	} {
		if !IsBogon(netip.MustParseAddr(s)) {
			t.Errorf("IsBogon(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "114.114.114.114", "2400:3200::1", "::ffff:8.8.8.8"} {
		if IsBogon(netip.MustParseAddr(s)) {
			t.Errorf("IsBogon(%s) = true, want false", s)
		}
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewCache(2, time.Hour, time.Minute)
	c.set("a", Location{City: "A"}, true)
	c.set("b", Location{City: "B"}, true)
	c.get("a") // a 变为最近使用
	c.set("c", Location{City: "C"}, true)
	if _, _, ok := c.get("b"); ok {
		t.Fatal("b 应被淘汰")
	}
	for _, key := range []string{"a", "c"} {
		if _, _, ok := c.get(key); !ok {
			t.Fatalf("%s 应在缓存中", key)
		}
	}
	if st := c.Stats(); st.Size != 2 || st.Hits != 3 || st.Misses != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestCacheExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewCache(10, time.Hour, time.Minute)
		c.set("found", Location{City: "A"}, true)
		c.set("miss", Location{}, false)
		if _, found, ok := c.get("miss"); !ok || found {
			t.Fatal("查无数据的结果应被缓存")
		}
		time.Sleep(2 * time.Minute)
		if _, _, ok := c.get("miss"); ok {
			t.Fatal("查无数据的结果应在 missTTL 后过期")
		}
		if loc, found, ok := c.get("found"); !ok || !found || loc.City != "A" {
			t.Fatal("有数据的结果应仍在缓存中")
		}
		time.Sleep(time.Hour)
		if _, _, ok := c.get("found"); ok {
			t.Fatal("有数据的结果应在 ttl 后过期")
		}
	})
}

func TestNilCache(t *testing.T) {
	c := NewCache(0, time.Hour, time.Hour)
	c.set("a", Location{}, true)
	if _, _, ok := c.get("a"); ok {
		t.Fatal("nil 缓存不应命中")
	}
	if st := c.Stats(); st != (CacheStats{}) {
		t.Fatalf("stats = %+v", st)
	}
}
