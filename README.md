## geoip-hub

轻量的 IP 地理位置查询聚合服务：本地 **GeoLite2**、**ip2region** 与公共 API（**ip-api.com**、**ipwho.is**）按查询链依次降级，
远程 API 自带缓存、请求合并、限流、429 退避和熔断，同一个 IP 被频繁查询也不会打爆上游或被拉黑。

- 只依赖标准库 `net/http`，不再使用 gin；镜像基于 scratch，二进制约 7MB，常驻内存约 13MB
- 查询链可配置，例如：ip-api 优先 → 失败/限流时国内 IP 用 ip2region → 海外 IP 用 GeoLite2
- 核心逻辑在 `geoip` 包中，其他 Go 项目可以直接 `import` 使用
- 兼容旧版接口与返回格式，新增运营商、ASN、经纬度、时区等字段，以及批量查询和运行统计

## 快速开始

1. 准备数据文件，放在 `docker-compose.yml` 同目录（至少一个，缺少的数据源启动时会自动跳过）：

   | 文件 | 说明 | 下载 |
   |---|---|---|
   | `GeoLite2-City.mmdb` | 全球城市级数据，多语言 | 注册 MaxMind 免费账号下载，[教程](http://www.modsecurity.cn/practice/post/15.html)；也兼容 [DB-IP City Lite](https://db-ip.com/db/download/ip-to-city-lite) |
   | `GeoLite2-ASN.mmdb` | 可选，补充 ASN 与网络归属 | 同上 |
   | `ip2region_v4.xdb` / `ip2region_v6.xdb` | 国内数据最准（省、市、运营商） | [lionsoul2014/ip2region/data](https://github.com/lionsoul2014/ip2region/tree/master/data)，旧版 `ip2region.xdb` 也支持 |

   数据文件都以只读 mmap 方式打开，只有访问到的数据页才占用内存，且内存紧张时可被系统回收：
   4 个文件全部加载时常驻内存约 13MB，最低配的机器也能跑。

2. 在 `docker-compose.yml` 中挂载已有的文件，然后启动：

   ```bash
   docker compose up -d
   ```

   数据文件更新后执行 `docker compose restart` 即可（启动不到 1 秒）。

## 接口

所有查询接口都支持 GET 和 POST，参数可以放在 query、表单或 JSON body 中。与旧版一致，HTTP 状态码始终为 200，结果看 `code`。

```bash
curl http://localhost:8080/8.8.8.8              # 查询指定 IP
curl http://localhost:8080/?ip=8.8.8.8          # 同上
curl http://localhost:8080/                     # 查询客户端自己的 IP
curl http://localhost:8080/ip                   # 只返回客户端 IP（纯文本），加 ?json=true 返回 JSON
curl "http://localhost:8080/120.229.10.10?source=ip2region&lang=en"   # 指定数据源、语言

# 批量查询，结果按顺序返回，单次最多 100 个
curl -X POST http://localhost:8080/batch -d '["8.8.8.8", "120.229.10.10", "2400:3200::1"]'

curl http://localhost:8080/stats                # 运行统计：缓存命中、各数据源请求/失败/限流次数、熔断状态
curl http://localhost:8080/healthz              # 健康检查，返回 ok
```

返回示例：

```json
{"code":200,"ip":"8.8.8.8","country":"美国","country_code":"US","region":"弗吉尼亚州","city":"Ashburn","isp":"Google LLC","asn":15169,"org":"Google Public DNS","latitude":39.03,"longitude":-77.5,"timezone":"America/New_York","source":"ipapi"}

{"code":200,"ip":"120.229.10.10","country":"中国","country_code":"CN","region":"广东省","city":"深圳市","isp":"移动","source":"ip2region"}

{"code":400,"ip":"abc","message":"Invalid IP address"}
```

| 字段 | 说明 |
|---|---|
| `code` | 200 成功，400 参数错误，500 所有数据源都失败 |
| `country` / `region` / `city` | 国家 / 省或州 / 城市，始终返回，没有数据时为空字符串 |
| `country_code` | ISO 3166-1 两位代码 |
| `isp` / `asn` / `org` | 运营商 / 自治系统号 / 网络所属组织 |
| `latitude` / `longitude` / `timezone` | 经纬度与时区 |
| `bogon` | 内网、回环等保留地址，为 `true` 时不会查询任何数据源 |
| `source` | 结果来源：`ipapi`、`ipwhois`、`ip2region`、`geolite` |

语言依次取 `lang` 参数、`Accept-Language` 请求头、`GEOIP_LANG`，支持 `zh-CN`、`en`、`ja`、`de`、`fr`、`ru`、`es`、`pt-BR`。
ip2region 的数据不区分语言（国内为中文、海外为英文）。

## 查询链与降级

`GEOIP_CHAIN` 是逗号分隔的数据源列表，按顺序查询，某个数据源出错、超时、被限流、熔断或没有该 IP 的数据时，自动查询下一个。

`名称@国家代码` 表示该数据源只对这个国家的 IP 直接采用。比如 `ip2region@CN`：国内 IP 直接用 ip2region 的结果；
海外 IP 交给后面的数据源，后面都查不到时再用 ip2region 的结果兜底。

| 配置 | 效果 |
|---|---|
| `ipapi,ip2region@CN,geolite`（默认） | ip-api 优先；它不可用时，国内 IP 用 ip2region，海外 IP 用 GeoLite2 |
| `ip2region@CN,ipapi,geolite` | 国内 IP 直接用 ip2region，不消耗 API 额度；海外 IP 先查 ip-api |
| `ip2region@CN,geolite` | 纯本地，不访问任何外部服务，单次查询约 2µs |
| `ipapi,ipwhois,ip2region@CN,geolite` | 两个 API 轮流兜底 |

## 远程 API 的保护机制

| 数据源 | 免费额度 | 默认限速 | 备注 |
|---|---|---|---|
| `ipapi`（ip-api.com） | 45 次/分钟 | `40/1m` | 免费版仅限非商业用途、只支持 HTTP；设置 `GEOIP_IPAPI_KEY` 后使用 pro 版（HTTPS） |
| `ipwhois`（ipwho.is） | 1000 次/天 | `900/24h` | HTTPS，允许商业使用；设置 `GEOIP_IPWHOIS_KEY` 后使用 ipwhois.pro |

- **缓存**：结果按「数据源 + 语言 + IP」缓存在内存 LRU 中，默认 24 小时；上游明确返回「查无数据」的也会缓存 10 分钟。
- **请求合并**：同一个 IP 的并发请求只会发出一次上游请求，其余请求等待共享结果。
- **限流**：每个 API 一个令牌桶，没有令牌时**立即降级**到下一个数据源，不排队，所以请求延迟不受影响。
- **退避**：收到 429 时按 `Retry-After` 暂停；ip-api 响应头 `X-Rl` 显示额度用完时，按 `X-Ttl` 主动暂停，避免被封 IP。
- **熔断**：连续失败 5 次后暂停 30 秒；恢复后再失败一次立即重新熔断，且暂停时间翻倍（最长 10 分钟），
  API 长时间不可用时，用来探测的请求很少，几乎不会拖慢正常请求。

本地数据源查询只需微秒级，不经过缓存。

## 配置

全部通过环境变量配置，都有默认值：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GEOIP_LISTEN` | `:8080` | 监听地址 |
| `GEOIP_CHAIN` | `ipapi,ip2region@CN,geolite` | 查询链，见上文 |
| `GEOIP_LANG` | `zh-CN` | 默认语言 |
| `GEOIP_GEOLITE_CITY` | `GeoLite2-City.mmdb` | GeoLite2 / GeoIP2 / DB-IP 的 City 库 |
| `GEOIP_GEOLITE_ASN` | `GeoLite2-ASN.mmdb` | 可选 |
| `GEOIP_IP2REGION_V4` | `ip2region_v4.xdb` | |
| `GEOIP_IP2REGION_V6` | `ip2region_v6.xdb` | 可选 |
| `GEOIP_CACHE_SIZE` | `100000` | 远程结果缓存条数，`0` 表示不缓存 |
| `GEOIP_CACHE_TTL` | `24h` | 有数据的结果缓存多久 |
| `GEOIP_CACHE_MISS_TTL` | `10m` | 查无数据的结果缓存多久 |
| `GEOIP_TIMEOUT` | `3s` | 远程请求超时 |
| `GEOIP_IPAPI_RATE` / `GEOIP_IPWHOIS_RATE` | `40/1m` / `900/24h` | 限速，格式为「次数/时长」，突发容量取次数的 1/10（1~10），`0` 表示不限速 |
| `GEOIP_IPAPI_KEY` / `GEOIP_IPWHOIS_KEY` | | 付费版密钥 |
| `GEOIP_TRUSTED_PROXIES` | `0.0.0.0/0,::/0` | 可信代理（CIDR 或 IP），只有直连地址可信时才读取 `X-Forwarded-For` / `X-Real-IP`；`none` 表示都不信任 |
| `GEOIP_BATCH_MAX` | `100` | 批量查询单次最多 IP 数 |
| `HTTP_PROXY` / `HTTPS_PROXY` | | 访问远程 API 使用的代理 |
| `TZ` | `UTC` | 时区（IANA 时区名，如 `Asia/Shanghai`），只影响日志时间 |

## 作为 Go 库使用

```bash
go get github.com/gaoyaxuan/geoip-hub
```

```go
geo, _ := geoip.OpenGeoLite("GeoLite2-City.mmdb", "GeoLite2-ASN.mmdb")
i2r, _ := geoip.OpenIP2Region("ip2region_v4.xdb", "ip2region_v6.xdb")
api := geoip.NewRemote(geoip.IPAPI, geoip.RemoteOptions{
	Limit: rate.Every(1500 * time.Millisecond), // 每分钟 40 次
	Burst: 4,
	Cache: geoip.NewCache(100_000, 24*time.Hour, 10*time.Minute),
})

r := geoip.NewResolver(
	geoip.Step{Provider: api},
	geoip.Step{Provider: i2r, Country: "CN"},
	geoip.Step{Provider: geo},
)
defer r.Close()

loc, err := r.Lookup(ctx, netip.MustParseAddr("8.8.8.8"), "zh-CN")
```

接入其他 API 只需定义一个 `geoip.API`（请求地址 + 响应解析），交给 `geoip.NewRemote` 就能获得同样的缓存、限流与熔断；
自定义数据源实现 `geoip.Provider` 接口即可加入查询链。

## 开发

```bash
go test -race ./...
GEOIP_TESTDATA=/path/to/data go test ./...                      # 用真实数据文件跑集成测试
GEOIP_TESTDATA=/path/to/data go test -bench . -run '^$' ./geoip/  # 基准测试
```
