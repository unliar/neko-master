// Command neko 是 neko-master 的 Go 后端。
//
// 用法：
//
//	neko                      # 启动 API + 采集
//	neko -seed-name home -seed-url 192.168.1.1:9090 -seed-token xxx
//	                          # 首次启动时写入一个默认 backend
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/api"
	"github.com/unliar/neko-master/neko-master-go/internal/collector"
	"github.com/unliar/neko-master/neko-master-go/internal/config"
	"github.com/unliar/neko-master/neko-master-go/internal/store"
)

func main() {
	var (
		seedName  = flag.String("seed-name", "", "首次启动时创建的 backend 名称")
		seedURL   = flag.String("seed-url", "", "backend 控制接口地址，如 192.168.1.1:9090")
		seedToken = flag.String("seed-token", "", "backend secret")
		pollMs    = flag.Int("poll-ms", 2000, "网关轮询间隔（毫秒）")
	)
	flag.Parse()

	cfg := config.Load()
	log := newLogger(cfg.LogLevel)

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("open database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()
	log.Info("database ready", "path", db.Path())

	// 首次启动时写入默认 backend（来自 flag 或环境变量）
	name := *seedName
	url := *seedURL
	token := *seedToken
	if name == "" {
		name = os.Getenv("SEED_BACKEND_NAME")
	}
	if url == "" {
		url = os.Getenv("SEED_BACKEND_URL")
	}
	if token == "" {
		token = os.Getenv("SEED_BACKEND_TOKEN")
	}
	if name != "" && url != "" {
		id, err := db.UpsertBackend(name, url, token, "clash")
		if err != nil {
			log.Error("seed backend", "err", err)
		} else {
			log.Info("backend seeded", "id", id, "name", name, "url", url)
		}
	}

	// 采集调度
	writer := store.NewWriter(db)
	runner := collector.NewRunner(
		log,
		writer,
		db,
		time.Duration(*pollMs)*time.Millisecond,
		time.Duration(cfg.FlushIntervalMS)*time.Millisecond,
		cfg.FlushMaxBuffer,
		cfg.BackendTLSInsecure,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runner.Start(ctx)

	// HTTP 服务
	srv := api.NewServer(cfg, db, log)
	httpSrv := &http.Server{
		Addr:              "0.0.0.0:" + cfg.APIPort,
		Handler:           loggingMiddleware(log, srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("api listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api serve", "err", err)
			cancel()
		}
	}()

	// 等待信号
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case <-ctx.Done():
		log.Info("shutting down", "reason", "context")
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	// 给采集器一点时间做最后刷盘
	time.Sleep(300 * time.Millisecond)
	log.Info("bye")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}

// loggingMiddleware 记录慢请求与错误。
func loggingMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		if rec.status >= 500 {
			log.Error("http", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "dur", dur.String())
		} else if dur > 2*time.Second {
			log.Warn("http slow", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "dur", dur.String())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack 转发底层连接劫持能力，WebSocket 升级依赖它。
// 不实现此方法会导致 /ws 握手退化为普通 HTTP 200。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

// Flush 转发 Flush，SSE/流式响应依赖。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
