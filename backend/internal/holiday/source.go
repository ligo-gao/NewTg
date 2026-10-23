// Package holiday 维护中国法定节假日与调休上班日日历（spec §5.2.1）。
//
// 【数据源】NateScarlet/holiday-cn（MIT）：每年一个 JSON，同时给出
// isOffDay=true 的法定假日与 isOffDay=false 的调休上班日——调休年份必须
// 区分这两类，否则「周六上班」会被当成周末低谷、「节假日」会被当成工作日。
// 默认经 jsDelivr 缓存拉取（见 config.HolidaySyncURLTemplate 的默认值）。
//
// 【两条职责分离】
//   - source.go：纯拉取 + 解析 + 完整性校验，不碰数据库；
//   - store.go：holiday_calendar 的「整年替换」与按日查询。
//
// 完整性校验的取舍：只有整年 JSON 完整解析成功才允许替换该年的行
// （ReplaceYear 在单个事务里先删后插）。空 days、重复日期、日期越界、
// year 字段与请求年份不一致都按失败处理——把半份数据写进日历会让调休日
// 静默按错误规则计价，比「今年没有数据、按星期规则继续」更危险。
//
// 计价侧永远拿到「查询失败」而不是错误：pricingstrategy.MultiplierAt 在
// LookupHoliday 返回 error 时回退到星期规则并标记 HolidayLookupFailed，
// 不会让请求失败或退化为免费（Global Constraints）。
package holiday

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SourceName 是写入 holiday_calendar.source 的数据源标识。
const SourceName = "holiday-cn"

// maxBodyBytes 是单个年度 JSON 的读取上限。真实文件约 3–10 KB；
// 上限只用于防御异常响应，避免把错误页/超大文件读进内存。
const maxBodyBytes = 1 << 20 // 1 MiB

// fetchRetryDelays 是 HTTP/传输失败后的重试等待（spec §5.2.1 的
// 「失败重试 3 次（指数退避）」：1 次首发 + len(delays) 次重试）。
//
// 是包级变量而不是常量：测试把它置空，避免用例为退避 sleep 买单。
var fetchRetryDelays = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}

// defaultClientTimeout 是未显式传入 *http.Client 时使用的超时（spec 要求 10s）。
const defaultClientTimeout = 10 * time.Second

// Day 是 holiday-cn 的一条年度事实。
type Day struct {
	// Date 是 YYYY-MM-DD（数据源原样字符串，已通过 time.Parse 校验）。
	Date string
	// Name 是节日名（调休上班日沿用它所属的节日名）。
	Name string
	// OffDay=true 是法定假日；false 是调休上班日。
	OffDay bool
}

// payload 是 holiday-cn 的年度 JSON 形状。
type payload struct {
	Year int      `json:"year"`
	Days []rawDay `json:"days"`
}

type rawDay struct {
	Name     string `json:"name"`
	Date     string `json:"date"`
	IsOffDay bool   `json:"isOffDay"`
}

// ErrNoDays 表示年度数据为空。空集合不是「今年没有假日」，而是数据源异常
// 或代理返回了错误页；按失败处理以保留上一次的好数据。
var ErrNoDays = errors.New("holiday: 年度数据没有任何日期")

// FetchYear 拉取并校验某一年的节假日日历。
//
// client 为 nil 时使用 10s 超时的默认客户端（调用方通常传入自建客户端以
// 复用连接池）。urlTemplate 必须恰好包含一个 %d 占位符，例如
// `https://cdn.jsdelivr.net/gh/NateScarlet/holiday-cn@master/%d.json`。
//
// 失败语义：HTTP 5xx/429 与传输错误按 fetchRetryDelays 指数退避重试，其它
// 状态码（4xx）和解析/校验错误立即返回；错误信息只含状态码与原因，不透传
// 响应体（运行历史保留 30 天，不能把上游 HTML/凭据写进去）。
func FetchYear(ctx context.Context, client *http.Client, urlTemplate string, year int) ([]Day, error) {
	if year < 1900 || year > 9999 {
		return nil, fmt.Errorf("holiday: 年份 %d 超出可拉取范围", year)
	}
	url, err := yearURL(urlTemplate, year)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: defaultClientTimeout}
	}

	var lastErr error
	for attempt := 0; attempt <= len(fetchRetryDelays); attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, fetchRetryDelays[attempt-1]); err != nil {
				return nil, err
			}
		}
		days, retryable, err := fetchOnce(ctx, client, url, year)
		if err == nil {
			return days, nil
		}
		lastErr = err
		if !retryable || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// fetchOnce 执行一次请求。retryable 表示该错误值得退避重试。
func fetchOnce(ctx context.Context, client *http.Client, url string, year int) ([]Day, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("holiday: 构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, fmt.Errorf("holiday: 请求 %s 失败: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("holiday: %s 返回 HTTP %d", url, resp.StatusCode)
	default:
		return nil, false, fmt.Errorf("holiday: %s 返回 HTTP %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, true, fmt.Errorf("holiday: 读取响应失败: %w", err)
	}
	if len(body) > maxBodyBytes {
		return nil, false, fmt.Errorf("holiday: 响应超过 %d 字节上限", maxBodyBytes)
	}
	return parseYear(body, url, year)
}

// parseYear 解析并校验年度 JSON。wantYear 为 0 表示不校验年份。
func parseYear(body []byte, url string, wantYear int) ([]Day, bool, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false, fmt.Errorf("holiday: %s 不是合法的年度 JSON: %w", url, err)
	}
	if wantYear != 0 && p.Year != wantYear {
		return nil, false, fmt.Errorf("holiday: %s 的 year=%d，期望 %d", url, p.Year, wantYear)
	}
	days, err := normalizeDays(p)
	if err != nil {
		return nil, false, err
	}
	return days, false, nil
}

// normalizeDays 校验年度数据并转换成 Day 列表（按日期升序）。
//
// 校验项：year 与 days 非空、日期可解析且属于该年、同一年内日期不重复。
// 任何一项失败都返回错误，让 ReplaceYear 保留上一次的好数据。
func normalizeDays(p payload) ([]Day, error) {
	if p.Year < 1900 || p.Year > 9999 {
		return nil, fmt.Errorf("holiday: 年度 JSON 的 year=%d 非法", p.Year)
	}
	if len(p.Days) == 0 {
		return nil, ErrNoDays
	}
	days := make([]Day, 0, len(p.Days))
	seen := make(map[string]struct{}, len(p.Days))
	for i, raw := range p.Days {
		t, err := time.Parse("2006-01-02", raw.Date)
		if err != nil {
			return nil, fmt.Errorf("holiday: days[%d].date=%q 不是 YYYY-MM-DD", i, raw.Date)
		}
		if t.Year() != p.Year {
			return nil, fmt.Errorf("holiday: days[%d].date=%q 不属于 %d 年", i, raw.Date, p.Year)
		}
		if _, dup := seen[raw.Date]; dup {
			return nil, fmt.Errorf("holiday: days[%d].date=%q 重复", i, raw.Date)
		}
		seen[raw.Date] = struct{}{}
		days = append(days, Day{Date: raw.Date, Name: raw.Name, OffDay: raw.IsOffDay})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	return days, nil
}

// yearURL 把年度模板展开成具体 URL。
//
// 模板必须恰好有一个 %d。用 Replace 而不是 Sprintf：URL 里可能出现百分号
// 编码（%20 等），Sprintf 会把它们当成动词、产出 `%!s(MISSING)`。占位符
// 数量不对时必须发出请求前就拒绝——部署侧拼写错误应该在首轮任务里显式
// 失败，而不是每天静默拉错地址。
func yearURL(urlTemplate string, year int) (string, error) {
	if strings.Count(urlTemplate, "%d") != 1 {
		return "", fmt.Errorf("holiday: URL 模板 %q 必须恰好包含一个 %%d", urlTemplate)
	}
	return strings.Replace(urlTemplate, "%d", strconv.Itoa(year), 1), nil
}

// sleepCtx 在等待期间响应取消。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
