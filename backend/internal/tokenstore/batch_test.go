package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestBatchAbortsOnBackendFailure 覆盖 D1-4：基础设施错误不得被收进
// failed[] 伪装成 200 部分失败，而是中止整批并返回可识别的 ErrBackend。
func TestBatchAbortsOnBackendFailure(t *testing.T) {
	st := New(nil, nil, nil)
	infra := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused (SQLSTATE 08006)")
	calls := 0

	succeeded, failed, err := st.batch(context.Background(), 1, []int64{4, 5, 6}, func(int64) error {
		calls++
		return infra
	})

	if !errors.Is(err, ErrBackend) {
		t.Fatalf("err = %v，期望 ErrBackend", err)
	}
	// 错误链必须保留：调用侧仍可记录底层原因，只是不得下发给客户端。
	if !errors.Is(err, infra) {
		t.Fatalf("err = %v，未保留底层错误链", err)
	}
	if calls != 1 {
		t.Fatalf("batch 在基础设施故障后仍继续执行（calls=%d），期望立即中止", calls)
	}
	if succeeded != 0 || len(failed) != 0 {
		t.Fatalf("succeeded=%d failed=%v，期望 0/空", succeeded, failed)
	}
	if strings.Contains(fmt.Sprint(failed), "10.0.0.5") {
		t.Fatalf("failed 泄露了内网地址: %v", failed)
	}
}

// TestBatchCollectsDomainFailuresButAbortsOnInfra 覆盖 D1-4 的组合语义：
// 领域失败逐条收集，遇到基础设施故障立即中止，最终 error 为 ErrBackend，
// 已收集的 failed 只含白名单文案。
func TestBatchCollectsDomainFailuresButAbortsOnInfra(t *testing.T) {
	st := New(nil, nil, nil)
	infra := errors.New("pq: password authentication failed for host db.internal:5432")
	order := make([]int64, 0, 3)

	succeeded, failed, err := st.batch(context.Background(), 1, []int64{1, 2, 3}, func(id int64) error {
		order = append(order, id)
		if id == 3 {
			return infra
		}
		return ErrNotFound
	})

	if !errors.Is(err, ErrBackend) || !errors.Is(err, infra) {
		t.Fatalf("err = %v，期望包裹 ErrBackend 与底层错误", err)
	}
	if fmt.Sprint(order) != "[1 2 3]" {
		t.Fatalf("执行顺序 = %v，期望在 3 处中止", order)
	}
	if succeeded != 0 || len(failed) != 2 {
		t.Fatalf("succeeded=%d failed=%d，期望 0/2", succeeded, len(failed))
	}
	for _, f := range failed {
		if f.Detail != "token not found" {
			t.Fatalf("失败明细 = %q，期望领域文案", f.Detail)
		}
		if f.Name != "" {
			t.Fatalf("不存在令牌不得回填名称: %+v", f)
		}
	}
	if strings.Contains(fmt.Sprint(failed), "db.internal") || strings.Contains(fmt.Sprint(failed), "password") {
		t.Fatalf("failed 泄露了底层错误文本: %v", failed)
	}
}

// TestBatchFailureDetailWhitelist 钉死 failed[].detail 的文案白名单；
// default 分支（正常不可达）必须是固定文案，绝不返回 err.Error()。
func TestBatchFailureDetailWhitelist(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not found", ErrNotFound, "token not found"},
		{"conflict", ErrConflict, "token was modified elsewhere"},
		{"validation", &ValidationError{Field: "name", Message: "令牌名不能为空"}, "令牌名不能为空"},
		{"infra", errors.New("write tcp 127.0.0.1:5432: broken pipe; constraint api_tokens_pkey"), "internal error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := batchFailureDetail(tc.err); got != tc.want {
				t.Fatalf("batchFailureDetail = %q，期望 %q", got, tc.want)
			}
		})
	}
}
