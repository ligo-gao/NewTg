package perfquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout  = 5 * time.Second
	defaultCacheTTL = 15 * time.Second

	queryRangePath = "/api/v1/query_range"

	// maxResponseBytes 防止异常上游把内存打满；正常矩阵响应远小于此。
	maxResponseBytes = 8 << 20
)

var (
	// ErrUnknownQuery 表示查询名不在白名单内。此时不会发出任何 HTTP 请求。
	ErrUnknownQuery = errors.New("perfquery: unknown query")
	// ErrUnavailable 表示 PROMETHEUS_URL 为空，监控采集未接入（降级态）。
	ErrUnavailable = errors.New("perfquery: prometheus unavailable")
)

// Sample 是矩阵中的一个时间点。At 带时区（Prometheus 用 Unix 秒）。
type Sample struct {
	At    time.Time
	Value float64
}

// Config 是 New 的输入。零值有默认：Timeout 5s、CacheTTL 15s、Now time.Now。
type Config struct {
	BaseURL  string
	Timeout  time.Duration
	CacheTTL time.Duration
	Now      func() time.Time
}

// Client 是 Prometheus 查询代理。BaseURL 为空时是「不可用实现」而不是 nil：
// 调用方永远不判空，降级只体现在 Available() 与 ErrUnavailable 上。
type Client struct {
	baseURL string
	ttl     time.Duration
	http    *http.Client
	cache   *ttlCache
	labels  *labelCache
	// flights / labelFlights 把同键的并发查询合并成一次上游请求：一次页面
	// 刷新会并发发 27 条查询，多管理员同时打开页面时同一查询会被重复请求，
	// 命中同一个 TTL 桶也照样打到 Prometheus。缓存只能救「已经完成」的请求，
	// in-flight 表救「正在执行」的请求。
	flights      *inflightGroup
	labelFlights *inflightGroup
}

// New 构造客户端。空 BaseURL 合法（未接入监控）；永不返回 nil。
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		ttl:     ttl,
		http: &http.Client{
			Timeout: timeout,
			// Proxy 必须为 nil：部署环境常设 HTTP(S)_PROXY，默认 Transport 会把
			// 内网地址 http://prometheus:9090 也送进代理，导致整页指标 unknown。
			// 连接池按「单次刷新 27 条并发查询」留余量（同 host 全部复用）。
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		cache:        newTTLCache(ttl, now),
		labels:       newLabelCache(ttl, now),
		flights:      newInflightGroup(),
		labelFlights: newInflightGroup(),
	}
}

// Available 报告是否配置了 Prometheus。false 时 QueryRange 一律返回
// ErrUnavailable，调用方据此把数据源标成 unavailable。
func (c *Client) Available() bool {
	return c.baseURL != ""
}

// QueryRange 执行一条白名单查询，返回按时间升序的全部样本。
//
// start 与 end 都先截到 CacheTTL 桶边界再参与请求与缓存键（两者口径一致）：
// 页面按 now 现算窗口时 start 每刷新一次就前移，只截 end 会让缓存永不命中；
// 两端都截后，同一 15 秒桶内的重复刷新（含多管理员各自刷新）共享一次上游查询。
// 代价是窗口 start 最多提前不到一个 TTL（≤15s），对概览曲线无感。
func (c *Client) QueryRange(ctx context.Context, name string, start, end time.Time, step time.Duration) ([]Sample, error) {
	promQL, _, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownQuery, name)
	}
	if !c.Available() {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, name)
	}

	start = start.Truncate(c.ttl)
	end = end.Truncate(c.ttl)
	key := cacheKey(name, start, end, step)
	if samples, ok := c.cache.get(key); ok {
		return samples, nil
	}

	call, leader := c.flights.join(key)
	if !leader {
		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("perfquery: %s: %w", name, ctx.Err())
		}
		if call.err != nil {
			return nil, call.err
		}
		return cloneSamples(call.samples), nil
	}

	samples, err := c.fetch(ctx, name, promQL, start, end, step)
	if err == nil {
		c.cache.put(key, samples)
	}
	c.flights.finish(key, call, samples, nil, err)
	// leader 也拿副本：call.samples 会同时被等待者克隆，若把同一底层数组
	// 直接交给调用方，「调用方改写返回值」就会与克隆互相竞争。
	return cloneSamples(samples), err
}

// QueryRangeLabeled 执行一条按 instance 标签分组的白名单查询，返回
// instance → 按时间升序的样本。没有 instance 标签的序列被跳过：调用方
// （overview 的 gateways 区）只能把指标与心跳按 instance 对齐，无法对齐的
// 序列若摊平进来，会让「某副本的数字」变成所有副本之和。
//
// 缓存口径与 QueryRange 完全一致：两端 start/end 先截到 CacheTTL 桶边界，
// 同一个 key 函数（名字 + start + end + step）。两条查询共用同一 TTL 是
// 有意的——同一页面刷新同时读两类查询，缓存窗口错开没有收益。
func (c *Client) QueryRangeLabeled(ctx context.Context, name string, start, end time.Time, step time.Duration) (map[string][]Sample, error) {
	promQL, _, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownQuery, name)
	}
	if !c.Available() {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, name)
	}

	start = start.Truncate(c.ttl)
	end = end.Truncate(c.ttl)
	key := cacheKey(name, start, end, step)
	if series, ok := c.labels.get(key); ok {
		return series, nil
	}

	call, leader := c.labelFlights.join(key)
	if !leader {
		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("perfquery: %s: %w", name, ctx.Err())
		}
		if call.err != nil {
			return nil, call.err
		}
		return cloneLabeled(call.labeled), nil
	}

	series, err := c.fetchLabeled(ctx, name, promQL, start, end, step)
	if err == nil {
		c.labels.put(key, series)
	}
	c.labelFlights.finish(key, call, nil, series, err)
	return cloneLabeled(series), err
}

// fetch 发一次 GET /api/v1/query_range 并解析矩阵。
func (c *Client) fetch(ctx context.Context, name, promQL string, start, end time.Time, step time.Duration) ([]Sample, error) {
	api, err := c.fetchResult(ctx, name, promQL, start, end, step)
	if err != nil {
		return nil, err
	}
	return api.samples(name)
}

// fetchLabeled 与 fetch 共用同一次 HTTP 调用与响应外壳，只是按 instance 分组。
func (c *Client) fetchLabeled(ctx context.Context, name, promQL string, start, end time.Time, step time.Duration) (map[string][]Sample, error) {
	api, err := c.fetchResult(ctx, name, promQL, start, end, step)
	if err != nil {
		return nil, err
	}
	return api.labeled(name)
}

// fetchResult 发一次 GET /api/v1/query_range 并返回解析后的响应外壳。
func (c *Client) fetchResult(ctx context.Context, name, promQL string, start, end time.Time, step time.Duration) (apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+queryRangePath+"?"+queryValues(promQL, start, end, step).Encode(), nil)
	if err != nil {
		return apiResponse{}, fmt.Errorf("perfquery: %s: %w", name, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return apiResponse{}, fmt.Errorf("perfquery: %s: %w", name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return apiResponse{}, fmt.Errorf("perfquery: %s: %w", name, err)
	}

	if resp.StatusCode != http.StatusOK {
		var api apiResponse
		if json.Unmarshal(body, &api) == nil && api.Error != "" {
			return apiResponse{}, fmt.Errorf("perfquery: %s: %s", name, api.Error)
		}
		return apiResponse{}, fmt.Errorf("perfquery: %s: http %d: %s", name, resp.StatusCode, truncate(strings.TrimSpace(string(body)), 200))
	}

	var api apiResponse
	if err := json.Unmarshal(body, &api); err != nil {
		return apiResponse{}, fmt.Errorf("perfquery: %s: decode response: %w", name, err)
	}
	if api.Status != "success" {
		msg := api.Error
		if msg == "" {
			msg = fmt.Sprintf("status %q", api.Status)
		}
		return apiResponse{}, fmt.Errorf("perfquery: %s: %s", name, msg)
	}
	return api, nil
}

// queryValues 组装 query_range 参数。start/end 用 Unix 秒，step 用秒
// （Prometheus HTTP API 的约定）。
func queryValues(promQL string, start, end time.Time, step time.Duration) url.Values {
	values := url.Values{}
	values.Set("query", promQL)
	values.Set("start", strconv.FormatInt(start.Unix(), 10))
	values.Set("end", strconv.FormatInt(end.Unix(), 10))
	values.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	return values
}

// apiResponse 是 query_range 的响应外壳。查询出错时 status != "success"，
// error 是 Prometheus 的原文。
type apiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string         `json:"resultType"`
		Result     []matrixResult `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

type matrixResult struct {
	Metric map[string]string    `json:"metric"`
	Values [][2]json.RawMessage `json:"values"`
}

// samples 把矩阵里所有序列的点摊平并按时间升序。无标签查询的 PromQL 都用了
// sum/avg/min，正常只返回一条序列；target.up 可能多条，摊平后由调用方按
// 语义解读。
func (r apiResponse) samples(name string) ([]Sample, error) {
	samples := make([]Sample, 0)
	for _, series := range r.Data.Result {
		for _, pair := range series.Values {
			at, err := parseTimestamp(pair[0])
			if err != nil {
				return nil, fmt.Errorf("perfquery: %s: bad timestamp %s: %w", name, pair[0], err)
			}
			value, err := parseValue(pair[1])
			if err != nil {
				return nil, fmt.Errorf("perfquery: %s: bad value %s: %w", name, pair[1], err)
			}
			samples = append(samples, Sample{At: at, Value: value})
		}
	}
	sort.SliceStable(samples, func(i, j int) bool {
		return samples[i].At.Before(samples[j].At)
	})
	return samples, nil
}

// labeled 按 series.Metric["instance"] 分组。没有 instance 标签的序列被
// 跳过（无法与副本心跳对齐）；同一 instance 的点按时间升序。
func (r apiResponse) labeled(name string) (map[string][]Sample, error) {
	labeled := make(map[string][]Sample)
	for _, series := range r.Data.Result {
		instance := strings.TrimSpace(series.Metric["instance"])
		if instance == "" {
			continue
		}
		for _, pair := range series.Values {
			at, err := parseTimestamp(pair[0])
			if err != nil {
				return nil, fmt.Errorf("perfquery: %s: bad timestamp %s: %w", name, pair[0], err)
			}
			value, err := parseValue(pair[1])
			if err != nil {
				return nil, fmt.Errorf("perfquery: %s: bad value %s: %w", name, pair[1], err)
			}
			labeled[instance] = append(labeled[instance], Sample{At: at, Value: value})
		}
	}
	for _, samples := range labeled {
		sort.SliceStable(samples, func(i, j int) bool {
			return samples[i].At.Before(samples[j].At)
		})
	}
	return labeled, nil
}

func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	seconds, err := parseFloatRaw(raw)
	if err != nil {
		return time.Time{}, err
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * float64(time.Second))
	return time.Unix(whole, nanos), nil
}

func parseValue(raw json.RawMessage) (float64, error) {
	return parseFloatRaw(raw)
}

// parseFloatRaw 同时接受 JSON 数字与字符串（Prometheus 的时间戳是数字，
// 样本值是字符串）。
func parseFloatRaw(raw json.RawMessage) (float64, error) {
	literal := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return strconv.ParseFloat(literal, 64)
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// inflightCall 是同一个缓存键上正在进行的一次上游查询。leader 负责执行
// fetch，其余调用者等到 done 关闭后拿同一结果（各自深拷贝）。
type inflightCall struct {
	done    chan struct{}
	samples []Sample
	labeled map[string][]Sample
	err     error
	// waiters 是合并进本次调用的等待者数量，排障时可据此确认合并是否生效。
	waiters int
}

// inflightGroup 把同键的并发查询合并成一次上游请求。与 Client 同生命周期：
// 条目在调用结束时删除，不引入后台 goroutine，也不缓存失败结果（失败后
// 下一个调用者会重新发请求，错误不会被放大成整段时间的失败）。
type inflightGroup struct {
	mu    sync.Mutex
	calls map[string]*inflightCall
}

func newInflightGroup() *inflightGroup {
	return &inflightGroup{calls: make(map[string]*inflightCall)}
}

// join 返回该键上的 in-flight 调用；leader=true 表示本次调用者负责执行。
func (g *inflightGroup) join(key string) (*inflightCall, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if call, ok := g.calls[key]; ok {
		call.waiters++
		return call, false
	}
	call := &inflightCall{done: make(chan struct{})}
	g.calls[key] = call
	return call, true
}

// finish 记录结果、移除条目并唤醒等待者。字段写入在 close(done) 之前完成，
// 等待者通过 channel 关闭获得 happens-before，无需再读锁。
func (g *inflightGroup) finish(key string, call *inflightCall, samples []Sample, labeled map[string][]Sample, err error) {
	g.mu.Lock()
	call.samples = samples
	call.labeled = labeled
	call.err = err
	delete(g.calls, key)
	g.mu.Unlock()
	close(call.done)
}
