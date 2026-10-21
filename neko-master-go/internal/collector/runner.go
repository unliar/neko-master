package collector

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
	"github.com/unliar/neko-master/neko-master-go/internal/model"
	"github.com/unliar/neko-master/neko-master-go/internal/realtime"
	"github.com/unliar/neko-master/neko-master-go/internal/store"
)

// BackendSource 提供当前需要采集的 backend 列表。
type BackendSource interface {
	// ActiveTargets 返回需要采集的 backend。
	ActiveTargets() ([]model.Target, error)
}

// Runner 为每个 backend 维护一个采集循环 + 缓冲 + 定时刷盘。
type Runner struct {
	log        *slog.Logger
	writer     *store.Writer
	src        BackendSource
	interval   time.Duration // 采集轮询间隔
	flushEvery time.Duration // 刷盘间隔
	maxBuffer  int
	insecure   bool
	realtime   *realtime.Manager

	mu      sync.Mutex
	workers map[int64]*worker
}

// worker 是单个 backend 的采集协程状态。
type worker struct {
	target  model.Target
	tracker *Tracker
	surge   *SurgeTracker
	buffer  *Buffer
	rt      *realtime.Store
	cancel  context.CancelFunc
}

// NewRunner 创建采集调度器。
func NewRunner(log *slog.Logger, w *store.Writer, src BackendSource,
	interval, flushEvery time.Duration, maxBuffer int, insecure bool) *Runner {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if flushEvery <= 0 {
		flushEvery = 30 * time.Second
	}
	return &Runner{
		log:        log,
		writer:     w,
		src:        src,
		interval:   interval,
		flushEvery: flushEvery,
		maxBuffer:  maxBuffer,
		insecure:   insecure,
		realtime:   realtime.NewManager(cfgRealtimeMinutes),
		workers:    make(map[int64]*worker),
	}
}

// cfgRealtimeMinutes 是 realtime buffer 默认保留的分钟桶数。
// 真正的值来自 config（REALTIME_MAX_MINUTES），
// 这里作为新建 Runner 时的默认。
const cfgRealtimeMinutes = 180

// Realtime 返回实时叠加管理器（供 stats 层使用）。
func (r *Runner) Realtime() *realtime.Manager { return r.realtime }

// Start 启动调度：定期对账 backend 列表，增删采集协程。
func (r *Runner) Start(ctx context.Context) {
	// 首次立即同步
	r.sync(ctx)

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.stopAll()
			return
		case <-tick.C:
			r.sync(ctx)
		}
	}
}

// sync 对齐当前 backend 列表与运行中的 worker。
func (r *Runner) sync(ctx context.Context) {
	targets, err := r.src.ActiveTargets()
	if err != nil {
		r.log.Error("collector: list backends", "err", err)
		return
	}

	want := make(map[int64]model.Target, len(targets))
	for _, t := range targets {
		// agent 模式由 agent 推数据，不主动拉
		if t.Type == "agent" {
			continue
		}
		want[t.ID] = t
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 新增
	for id, t := range want {
		if _, ok := r.workers[id]; ok {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		w := &worker{
			target:  t,
			tracker: NewTracker(),
			surge:   NewSurgeTracker(),
			buffer:  NewBuffer(id, r.maxBuffer),
			rt:      r.realtime.Store(id),
			cancel:  cancel,
		}
		r.workers[id] = w
		go r.runWorker(wctx, w)
		r.log.Info("collector: started", "backend", id, "name", t.Name, "url", t.URL, "type", t.Type)
	}

	// 删除
	for id, w := range r.workers {
		if _, ok := want[id]; ok {
			continue
		}
		w.cancel()
		// 最后一次刷盘；成功才清 realtime，失败保留以便下次启动后续写。
		if updates := w.buffer.Drain(); len(updates) > 0 {
			if err := r.writer.Flush(id, updates); err != nil {
				r.log.Error("collector: final flush", "backend", id, "err", err)
			} else {
				w.rt.ClearTraffic()
			}
		}
		r.realtime.Drop(id)
		delete(r.workers, id)
		r.log.Info("collector: stopped", "backend", id)
	}
}

// runWorker 是单个 backend 的采集 + 刷盘循环。
func (r *Runner) runWorker(ctx context.Context, w *worker) {
	// Surge 与 Clash 的采集入口、鉴权头、数据形态全不同，分开走。
	if strings.EqualFold(w.target.Type, "surge") {
		r.runSurgeWorker(ctx, w)
		return
	}

	client := gateway.New(w.target.URL, w.target.Token, r.insecure)

	poll := time.NewTicker(r.interval)
	defer poll.Stop()

	flush := time.NewTicker(r.flushEvery)
	defer flush.Stop()

	// 失败退避
	fails := 0
	var backoffUntil time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			r.flushOnce(w)
		case <-poll.C:
			if time.Now().Before(backoffUntil) {
				continue
			}
			now := time.Now()
			resp, err := client.Connections(ctx)
			if err != nil {
				fails++
				r.log.Warn("collector: connections failed",
					"backend", w.target.ID, "err", err, "fails", fails)
				// 指数退避，上限 60s
				backoff := time.Duration(1<<uint(min(fails, 6))) * time.Second
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
				backoffUntil = time.Now().Add(backoff)
				continue
			}
			fails = 0

			updates := w.tracker.Diff(resp, now)
			full := false
			for _, u := range updates {
				w.rt.Record(u)
				if w.buffer.Add(u) {
					full = true
				}
			}
			if full {
				r.flushOnce(w)
			}
		}
	}
}

// runSurgeWorker 是 Surge backend 的采集循环：轮询 /v1/requests/recent。
//
// 与 Clash 循环共享退避/缓冲/落盘逻辑，只有取数与做差两步不同。
func (r *Runner) runSurgeWorker(ctx context.Context, w *worker) {
	client := gateway.NewSurgeClient(w.target.URL, w.target.Token, r.insecure)

	poll := time.NewTicker(r.interval)
	defer poll.Stop()

	flush := time.NewTicker(r.flushEvery)
	defer flush.Stop()

	fails := 0
	var backoffUntil time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			r.flushOnce(w)
		case <-poll.C:
			if time.Now().Before(backoffUntil) {
				continue
			}
			now := time.Now()
			data, err := client.RequestsRecent(ctx)
			if err != nil {
				fails++
				r.log.Warn("collector: surge requests failed",
					"backend", w.target.ID, "err", err, "fails", fails)
				backoff := time.Duration(1<<uint(min(fails, 6))) * time.Second
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
				backoffUntil = time.Now().Add(backoff)
				continue
			}
			fails = 0

			updates := w.surge.Diff(data, now)
			full := false
			for _, u := range updates {
				w.rt.Record(u)
				if w.buffer.Add(u) {
					full = true
				}
			}
			if full {
				r.flushOnce(w)
			}
		}
	}
}

// flushOnce 把缓冲区落库。
//
// realtime 清理时机对齐上游 gateway.collector.ts 的 flushBatch：
//
//	落盘成功 → clearTraffic()，否则保留缓冲供下次重试。
//
// 上游在 ClickHouse 开启时会把 detail / agg 分开判定（只成功一半时只清对应侧）。
// Go 侧目前 ClickHouse 是纯旁路且写入失败不影响主流程，
// 因此这里只按 SQLite 事务结果决定：SQLite 成功即清，失败则不清。
func (r *Runner) flushOnce(w *worker) {
	updates := w.buffer.Drain()
	if len(updates) == 0 {
		return
	}
	if err := r.writer.Flush(w.target.ID, updates); err != nil {
		// 上游注释原话：retaining realtime store。
		// 不清 buffer，下次 flush 会带着这批数据重试。
		r.log.Error("collector: flush failed, retaining realtime store",
			"backend", w.target.ID, "count", len(updates), "err", err)
		return
	}
	w.rt.ClearTraffic()
	r.log.Debug("collector: flushed", "backend", w.target.ID, "count", len(updates))
}

// stopAll 停止全部 worker。
func (r *Runner) stopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.workers {
		w.cancel()
		if updates := w.buffer.Drain(); len(updates) > 0 {
			if err := r.writer.Flush(id, updates); err == nil {
				w.rt.ClearTraffic()
			}
		}
		r.realtime.Drop(id)
	}
	r.workers = make(map[int64]*worker)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// NewWriterFor 直接用一个已打开的 DB 构造 Writer。
//
// 供 agent 上报路径使用（它没有 Runner）。
func NewWriterFor(db *store.DB) *store.Writer { return store.NewWriter(db) }
