package geoip

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

// IP2Region 是 ip2region 的 xdb 数据库，支持 IPv4 / IPv6，以及新旧两种数据格式。
// 文件以只读 mmap 方式打开，与 GeoLite2 一样只有访问到的页才占用内存。
type IP2Region struct {
	v4, v6 *xdbFile
}

type xdbFile struct {
	version *xdb.Version
	data    []byte
	unmap   func() error
	legacy  bool // 2.0 结构（仅 IPv4）：国家|区域|省份|城市|ISP；3.0 结构：国家|省份|城市|ISP|国家代码
	created time.Time
}

// OpenIP2Region 打开 xdb 文件，根据文件头自动识别 IPv4 / IPv6
func OpenIP2Region(paths ...string) (*IP2Region, error) {
	p := &IP2Region{}
	for _, path := range paths {
		f, err := openXDB(path)
		if err != nil {
			p.Close()
			return nil, err
		}
		slot := &p.v6
		if f.version.Id == xdb.IPv4VersionNo {
			slot = &p.v4
		}
		if *slot != nil {
			f.unmap()
			p.Close()
			return nil, fmt.Errorf("%s: 重复的 %s 数据文件", path, f.version.Name)
		}
		*slot = f
	}
	if p.v4 == nil && p.v6 == nil {
		return nil, errors.New("ip2region: 没有数据文件")
	}
	return p, nil
}

func openXDB(path string) (*xdbFile, error) {
	data, unmap, err := mapFile(path)
	if err != nil {
		return nil, err
	}
	f := &xdbFile{data: data, unmap: unmap}
	header, err := xdb.LoadHeaderFromBuff(data)
	if err == nil {
		f.version, err = xdb.VersionFromHeader(header)
	}
	// 最后一条段索引必须完整，否则是下载不完整的文件，查询时会越界
	if err == nil && int64(header.EndIndexPtr)+int64(f.version.SegmentIndexSize) > int64(len(data)) {
		err = errors.New("文件不完整，请重新下载")
	}
	if err != nil {
		unmap()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.legacy = header.Version == xdb.Structure20
	f.created = time.Unix(int64(header.CreatedAt), 0)
	return f, nil
}

func (p *IP2Region) Name() string { return "ip2region" }

func (p *IP2Region) Lookup(_ context.Context, ip netip.Addr, _ string) (Location, error) {
	ip = ip.Unmap()
	f := p.v6
	if ip.Is4() {
		f = p.v4
	}
	if f == nil {
		return Location{}, ErrNotFound
	}
	// Searcher 查询时会写内部计数器，不能并发共享；它只引用 data，每次新建的开销很小
	s, err := xdb.NewWithBuffer(f.version, f.data)
	if err != nil {
		return Location{}, err
	}
	region, err := s.Search(ip.AsSlice())
	if err != nil {
		return Location{}, err
	}
	loc := parseRegion(region, f.legacy)
	if loc.Country == "" {
		return Location{}, ErrNotFound
	}
	return loc, nil
}

// parseRegion 解析 xdb 中的区域字符串，"0" 表示空
func parseRegion(s string, legacy bool) Location {
	f := strings.Split(s, "|")
	for len(f) < 5 {
		f = append(f, "")
	}
	for i, v := range f {
		if v == "0" || v == "Reserved" || v == "内网IP" {
			f[i] = ""
		}
	}
	if legacy {
		loc := Location{Country: f[0], Region: f[2], City: f[3], ISP: f[4]}
		if loc.Country == "中国" {
			loc.CountryCode = "CN" // 旧数据没有国家代码，补上中国的，供 @CN 判断
		}
		return loc
	}
	return Location{Country: f[0], Region: f[1], City: f[2], ISP: f[3], CountryCode: f[4]}
}

// BuildTime 返回数据文件的生成时间（有多个文件时取最新的）
func (p *IP2Region) BuildTime() time.Time {
	var t time.Time
	for _, f := range []*xdbFile{p.v4, p.v6} {
		if f != nil && f.created.After(t) {
			t = f.created
		}
	}
	return t
}

// Close 解除文件映射，之后不能再查询
func (p *IP2Region) Close() error {
	var errs []error
	for _, f := range []*xdbFile{p.v4, p.v6} {
		if f != nil {
			errs = append(errs, f.unmap())
		}
	}
	return errors.Join(errs...)
}
