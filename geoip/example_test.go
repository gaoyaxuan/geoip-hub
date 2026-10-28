package geoip_test

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"time"

	"github.com/gaoyaxuan/geoip-hub/geoip"
	"golang.org/x/time/rate"
)

// 在其他 Go 项目中直接使用：ip-api 优先，失败、限流或熔断时降级到本地库，国内 IP 用 ip2region、海外用 GeoLite2
func Example() {
	geo, err := geoip.OpenGeoLite("GeoLite2-City.mmdb", "GeoLite2-ASN.mmdb")
	if err != nil {
		log.Fatal(err)
	}
	i2r, err := geoip.OpenIP2Region("ip2region_v4.xdb", "ip2region_v6.xdb")
	if err != nil {
		log.Fatal(err)
	}
	cache := geoip.NewCache(100_000, 24*time.Hour, 10*time.Minute)
	api := geoip.NewRemote(geoip.IPAPI, geoip.RemoteOptions{
		Limit: rate.Every(1500 * time.Millisecond), // 每分钟 40 次
		Burst: 4,
		Cache: cache,
	})

	r := geoip.NewResolver(
		geoip.Step{Provider: api},
		geoip.Step{Provider: i2r, Country: "CN"},
		geoip.Step{Provider: geo},
	)
	defer r.Close()

	loc, err := r.Lookup(context.Background(), netip.MustParseAddr("8.8.8.8"), "zh-CN")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(loc.Country, loc.Region, loc.City, loc.ISP, loc.Source)
}
