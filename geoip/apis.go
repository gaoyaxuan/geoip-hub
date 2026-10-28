package geoip

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// remoteLangs 是 ip-api 和 ipwho.is 都支持的语言
var remoteLangs = []string{"en", "zh-CN", "ja", "de", "fr", "ru", "es", "pt-BR"}

// IPAPI 是 ip-api.com：免费版每分钟 45 次、只支持 HTTP、仅限非商业用途；配置 key 后使用 pro 版（HTTPS、不限量）。
var IPAPI = API{
	Name:  "ipapi",
	Langs: remoteLangs,
	URL: func(ip netip.Addr, lang, key string) string {
		q := "?fields=status,message,country,countryCode,regionName,city,lat,lon,timezone,isp,org,as&lang=" + lang
		if key != "" {
			return "https://pro.ip-api.com/json/" + ip.String() + q + "&key=" + url.QueryEscape(key)
		}
		return "http://ip-api.com/json/" + ip.String() + q
	},
	Parse: func(body []byte) (Location, error) {
		var v struct {
			Status      string  `json:"status"`
			Message     string  `json:"message"`
			Country     string  `json:"country"`
			CountryCode string  `json:"countryCode"`
			RegionName  string  `json:"regionName"`
			City        string  `json:"city"`
			Lat         float64 `json:"lat"`
			Lon         float64 `json:"lon"`
			Timezone    string  `json:"timezone"`
			ISP         string  `json:"isp"`
			Org         string  `json:"org"`
			AS          string  `json:"as"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return Location{}, err
		}
		if v.Status != "success" {
			return Location{}, fmt.Errorf("%w: %s", ErrNotFound, v.Message)
		}
		asn, asOrg := parseASN(v.AS)
		return Location{
			Country: v.Country, CountryCode: v.CountryCode, Region: v.RegionName, City: v.City,
			ISP: v.ISP, ASN: asn, Org: cmp.Or(v.Org, asOrg),
			Latitude: v.Lat, Longitude: v.Lon, TimeZone: v.Timezone,
		}, nil
	},
	// X-Rl 是当前窗口剩余次数，X-Ttl 是窗口重置的剩余秒数；超限会返回 429，频繁超限会被封 IP 1 小时
	Exhausted: func(h http.Header) time.Duration {
		if h.Get("X-Rl") != "0" {
			return 0
		}
		ttl, _ := strconv.Atoi(h.Get("X-Ttl"))
		return time.Duration(ttl+1) * time.Second
	},
}

// IPWhois 是 ipwho.is：免费版支持 HTTPS、每天 1000 次、允许商业使用；配置 key 后使用 ipwhois.pro。
var IPWhois = API{
	Name:  "ipwhois",
	Langs: remoteLangs,
	URL: func(ip netip.Addr, lang, key string) string {
		q := "?fields=success,message,country,country_code,region,city,latitude,longitude,connection,timezone.id&lang=" + lang
		if key != "" {
			return "https://ipwhois.pro/" + ip.String() + q + "&key=" + url.QueryEscape(key)
		}
		return "https://ipwho.is/" + ip.String() + q
	},
	Parse: func(body []byte) (Location, error) {
		var v struct {
			Success     bool    `json:"success"`
			Message     string  `json:"message"`
			Country     string  `json:"country"`
			CountryCode string  `json:"country_code"`
			Region      string  `json:"region"`
			City        string  `json:"city"`
			Latitude    float64 `json:"latitude"`
			Longitude   float64 `json:"longitude"`
			Connection  struct {
				ASN uint   `json:"asn"`
				Org string `json:"org"`
				ISP string `json:"isp"`
			} `json:"connection"`
			Timezone struct {
				ID string `json:"id"`
			} `json:"timezone"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return Location{}, err
		}
		if !v.Success {
			return Location{}, fmt.Errorf("%w: %s", ErrNotFound, v.Message)
		}
		return Location{
			Country: v.Country, CountryCode: v.CountryCode, Region: v.Region, City: v.City,
			ISP: v.Connection.ISP, ASN: v.Connection.ASN, Org: v.Connection.Org,
			Latitude: v.Latitude, Longitude: v.Longitude, TimeZone: v.Timezone.ID,
		}, nil
	},
}

// parseASN 拆分 "AS15169 Google LLC" 形式的字符串
func parseASN(s string) (uint, string) {
	num, name, _ := strings.Cut(s, " ")
	n, err := strconv.ParseUint(strings.TrimPrefix(num, "AS"), 10, 32)
	if err != nil {
		return 0, s
	}
	return uint(n), name
}
