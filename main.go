// geoip-hub 是聚合 GeoLite2、ip2region 与公共 API 的 IP 地理位置查询服务。
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // 内嵌时区数据：scratch 镜像里没有时区库，设置 TZ 环境变量也能生效
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("服务异常退出", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	resolver, cache, err := buildResolver(cfg)
	if err != nil {
		return err
	}
	defer resolver.Close()

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           newServer(cfg, resolver, cache).routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	slog.Info("服务已启动", "listen", ln.Addr().String(), "chain", strings.Join(resolver.Chain(), " → "))

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	slog.Info("正在关闭服务")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
