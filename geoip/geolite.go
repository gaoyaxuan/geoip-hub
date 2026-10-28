package geoip

import (
	"cmp"
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/oschwald/geoip2-golang/v2"
)

// GeoLite 是 MaxMind GeoLite2 / GeoIP2 City 数据库（也兼容 DB-IP 的 City Lite），可以搭配 ASN 库补充网络归属
type GeoLite struct {
	city *geoip2.Reader
	asn  *geoip2.Reader
}

// OpenGeoLite 以 mmap 方式打开 City 库，asnPath 为空时不加载 ASN 库
func OpenGeoLite(cityPath, asnPath string) (*GeoLite, error) {
	city, err := geoip2.Open(cityPath)
	if err != nil {
		return nil, err
	}
	g := &GeoLite{city: city}
	if asnPath != "" {
		if g.asn, err = geoip2.Open(asnPath); err != nil {
			city.Close()
			return nil, err
		}
	}
	return g, nil
}

func (g *GeoLite) Name() string { return "geolite" }

func (g *GeoLite) Lookup(_ context.Context, ip netip.Addr, lang string) (Location, error) {
	c, err := g.city.City(ip)
	if err != nil {
		return Location{}, err
	}
	country := c.Country
	if !country.HasData() {
		country = c.RegisteredCountry // anycast 等地址只有注册国家
	}
	loc := Location{
		Country:     localName(country.Names, lang),
		CountryCode: country.ISOCode,
		City:        localName(c.City.Names, lang),
		TimeZone:    c.Location.TimeZone,
	}
	if loc.Country == "" {
		return Location{}, ErrNotFound
	}
	if len(c.Subdivisions) > 0 {
		loc.Region = localName(c.Subdivisions[0].Names, lang)
	}
	if c.Location.HasCoordinates() {
		loc.Latitude, loc.Longitude = *c.Location.Latitude, *c.Location.Longitude
	}
	if g.asn != nil {
		if a, err := g.asn.ASN(ip); err == nil {
			loc.ASN, loc.Org = a.AutonomousSystemNumber, a.AutonomousSystemOrganization
		}
	}
	return loc, nil
}

// localName 取指定语言的名称，缺失时回退到英文
func localName(n geoip2.Names, lang string) string {
	var name string
	switch lang {
	case "zh-CN":
		name = n.SimplifiedChinese
	case "ja":
		name = n.Japanese
	case "de":
		name = n.German
	case "fr":
		name = n.French
	case "ru":
		name = n.Russian
	case "es":
		name = n.Spanish
	case "pt-BR":
		name = n.BrazilianPortuguese
	}
	return cmp.Or(name, n.English)
}

// BuildTime 返回 City 库的构建时间
func (g *GeoLite) BuildTime() time.Time { return g.city.Metadata().BuildTime() }

func (g *GeoLite) Close() error {
	err := g.city.Close()
	if g.asn != nil {
		err = errors.Join(err, g.asn.Close())
	}
	return err
}
