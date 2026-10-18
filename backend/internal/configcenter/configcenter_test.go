package configcenter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是 BE-6 配置中心的单元测试：用 dbQuerier / redisClient 两个接口缝隙替代
// 真实 PG/Redis（GC-8：无 -tags=integration 时不得依赖外部服务）。
// 集成测试见 configcenter_integration_test.go。

type fakeResult struct{ affected int64 }

func (r fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.affected, nil }

type execCall struct {
	query string
	args  []any
}

// fakeDB 只实现 Save 单键路径所需的 ExecContext / QueryRowContext；
// QueryContext（Load 用）在单测中不应被调用。
type fakeDB struct {
	execCalls  []execCall
	execResult sql.Result
	execErr    error
	row        rowScanner
	rowCalls   int
}

func (f *fakeDB) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.execCalls = append(f.execCalls, execCall{query: query, args: args})
	return f.execResult, f.execErr
}

func (f *fakeDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("fakeDB: 单测不应调用 QueryContext")
}

func (f *fakeDB) QueryRowContext(context.Context, string, ...any) rowScanner {
	f.rowCalls++
	return f.row
}

type fakeRow struct {
	value     []byte
	updatedAt time.Time
	err       error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 1 {
		p, ok := dest[0].(*time.Time)
		if !ok {
			return errors.New("fakeRow: 单目标必须是 *time.Time")
		}
		*p = r.updatedAt
		return nil
	}
	if len(dest) == 2 {
		vp, ok := dest[0].(*[]byte)
		if !ok {
			return errors.New("fakeRow: 第一目标必须是 *[]byte")
		}
		tp, ok := dest[1].(*time.Time)
		if !ok {
			return errors.New("fakeRow: 第二目标必须是 *time.Time")
		}
		*vp = append([]byte(nil), r.value...)
		*tp = r.updatedAt
		return nil
	}
	return errors.New("fakeRow: 不支持的目标数量")
}

type publishedMsg struct {
	channel string
	message any
}

type fakeRedis struct {
	published  []publishedMsg
	publishErr error
}

func (f *fakeRedis) Publish(_ context.Context, channel string, message any) *redis.IntCmd {
	f.published = append(f.published, publishedMsg{channel: channel, message: message})
	return redis.NewIntResult(1, f.publishErr)
}

func (f *fakeRedis) Subscribe(context.Context, ...string) *redis.PubSub {
	panic("fakeRedis: 单测不应调用 Subscribe")
}

type fakeSealer struct{ prefix string }

func (s fakeSealer) Seal(plain string) (string, error) { return s.prefix + plain, nil }
func (s fakeSealer) Open(sealed string) (string, error) {
	return strings.TrimPrefix(sealed, s.prefix), nil
}

// TestSaveOptimisticLockConflict：affected==0 → ErrConflict，且冲突时不广播。
func TestSaveOptimisticLockConflict(t *testing.T) {
	expected := time.Date(2026, 10, 18, 12, 0, 0, 123456000, time.UTC)
	db := &fakeDB{execResult: fakeResult{affected: 0}}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb)

	err := c.Save(context.Background(), "site.title", "Token Gateway", &expected)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Save 冲突应返回 ErrConflict，实际 %v", err)
	}
	if len(db.execCalls) != 1 {
		t.Fatalf("期望 1 次 Exec，实际 %d", len(db.execCalls))
	}
	call := db.execCalls[0]
	if !strings.Contains(call.query, `WHERE "key" = $3 AND updated_at = $4`) {
		t.Fatalf("乐观锁 SQL 形状不符：%s", call.query)
	}
	if len(call.args) != 4 {
		t.Fatalf("期望 4 个参数，实际 %d", len(call.args))
	}
	if got, ok := call.args[2].(string); !ok || got != "site.title" {
		t.Fatalf("$3 应为 key，实际 %#v", call.args[2])
	}
	if got, ok := call.args[3].(time.Time); !ok || !got.Equal(expected) {
		t.Fatalf("$4 应为 expectUpdatedAt，实际 %#v", call.args[3])
	}
	if len(rdb.published) != 0 {
		t.Fatalf("冲突时不应 PUBLISH，实际 %d 条", len(rdb.published))
	}
	if db.rowCalls != 0 {
		t.Fatalf("冲突时不应触发单键重载，实际 %d 次", db.rowCalls)
	}
}

// TestSavePublishesFrozenChannel：成功后 PUBLISH 到冻结频道且 payload 为 key，
// 同时本地快照与版本被单键重载刷新。
func TestSavePublishesFrozenChannel(t *testing.T) {
	expected := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	newVersion := expected.Add(time.Second)
	db := &fakeDB{
		execResult: fakeResult{affected: 1},
		row:        fakeRow{value: []byte(`{"enabled":true}`), updatedAt: newVersion},
	}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb)

	value := map[string]any{"enabled": true}
	if err := c.Save(context.Background(), "site.demo", value, &expected); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}

	if len(rdb.published) != 1 {
		t.Fatalf("期望 1 条 PUBLISH，实际 %d", len(rdb.published))
	}
	if rdb.published[0].channel != ChangedChannel {
		t.Fatalf("频道应为 %q，实际 %q", ChangedChannel, rdb.published[0].channel)
	}
	if rdb.published[0].channel != "newtg:cfg:changed" {
		t.Fatalf("冻结频道名被改动：%q", rdb.published[0].channel)
	}
	if key, ok := rdb.published[0].message.(string); !ok || key != "site.demo" {
		t.Fatalf("PUBLISH payload 应为 key，实际 %#v", rdb.published[0].message)
	}

	got, ok := c.Get("site.demo")
	if !ok {
		t.Fatal("Save 成功后本地快照应包含该键")
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("快照值不是合法 JSON：%v", err)
	}
	if decoded["enabled"] != true {
		t.Fatalf("快照值不符：%s", got)
	}
	version, ok := c.Version("site.demo")
	if !ok || !version.Equal(newVersion) {
		t.Fatalf("版本应为 %v，实际 %v (ok=%v)", newVersion, version, ok)
	}

	// 返回的快照不得与内部存储共享可变底层数组。
	got[0] = '['
	again, _ := c.Get("site.demo")
	if len(again) == 0 || again[0] != '{' {
		t.Fatal("Get 应返回独立副本")
	}
}

// TestSaveUpsertWithoutExpectation：expectUpdatedAt==nil 走 upsert，无冲突语义。
func TestSaveUpsertWithoutExpectation(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	db := &fakeDB{row: fakeRow{updatedAt: now}}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb)

	if err := c.Save(context.Background(), "site.title", "New TG", nil); err != nil {
		t.Fatalf("无期望版本 Save 失败：%v", err)
	}
	if len(db.execCalls) != 0 {
		t.Fatalf("nil expectUpdatedAt 应走 upsert，不应执行 UPDATE，实际 %d 次", len(db.execCalls))
	}
	if db.rowCalls != 1 {
		t.Fatalf("upsert 应 1 次 QueryRow，实际 %d", db.rowCalls)
	}
	if len(rdb.published) != 1 || rdb.published[0].message != "site.title" {
		t.Fatalf("upsert 后应广播 key，实际 %#v", rdb.published)
	}
	if v, ok := c.Get("site.title"); !ok || string(v) != `"New TG"` {
		t.Fatalf("快照值应为 JSON 字符串，实际 %s (ok=%v)", v, ok)
	}
}

// TestSaveSecretKeyRequiresSealer：secretKeys 命中且 Sealer 为 nil 时拒绝写入。
func TestSaveSecretKeyRequiresSealer(t *testing.T) {
	db := &fakeDB{execResult: fakeResult{affected: 1}}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb)
	c.secretKeys = map[string]struct{}{"pay.stripe.secret": {}}

	err := c.Save(context.Background(), "pay.stripe.secret", "sk_live_x", nil)
	if !errors.Is(err, ErrSecretSealerRequired) {
		t.Fatalf("密钥类键无 Sealer 应返回 ErrSecretSealerRequired，实际 %v", err)
	}
	if len(db.execCalls) != 0 || db.rowCalls != 0 || len(rdb.published) != 0 {
		t.Fatal("拒绝写入时不应触达 DB 或 PUBLISH")
	}
}

// TestSaveSecretKeySealed：配置 Sealer 后密钥类键以密文落库。
func TestSaveSecretKeySealed(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	db := &fakeDB{row: fakeRow{updatedAt: now}}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb)
	c.secretKeys = map[string]struct{}{"pay.stripe.secret": {}}
	c.Sealer = fakeSealer{prefix: "sealed:"}

	if err := c.Save(context.Background(), "pay.stripe.secret", "sk_live_x", nil); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	if v, ok := c.Get("pay.stripe.secret"); !ok || string(v) != `"sealed:sk_live_x"` {
		t.Fatalf("落库值应为密文，实际 %s (ok=%v)", v, ok)
	}
}

// TestGetUnknownKey：未知键 ok=false。
func TestGetUnknownKey(t *testing.T) {
	c := newCenter(&fakeDB{}, &fakeRedis{})
	if _, ok := c.Get("nope.missing"); ok {
		t.Fatal("未知键 Get 应返回 ok=false")
	}
	if _, ok := c.Version("nope.missing"); ok {
		t.Fatal("未知键 Version 应返回 ok=false")
	}
}

// TestChangedKey：兼容本计划的纯 key payload 与 modules-08 §1.3 的 {key, ts} 对象。
func TestChangedKey(t *testing.T) {
	cases := map[string]string{
		"site.title":                           "site.title",
		`{"key":"site.title","ts":1729000000}`: "site.title",
		`{"ts":1729000000}`:                    `{"ts":1729000000}`,
		`not-json`:                             "not-json",
	}
	for payload, want := range cases {
		if got := changedKey(payload); got != want {
			t.Errorf("changedKey(%q) = %q，期望 %q", payload, got, want)
		}
	}
}
