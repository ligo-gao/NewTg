package store

// 批量建用户仓储的**纯单元测试**（不连 PostgreSQL）：只钉住 COMMIT 失败的
// 哨兵包装。真实事务的 COMMIT 失败无法在测试里可靠触发（需要让 PG 在提交
// 瞬间断开），所以把提交收口成 commitBulkTx 并用假 committer 固定契约。

import (
	"errors"
	"strings"
	"testing"
)

// failingCommitter 模拟 COMMIT 时连接断开的 *sql.Tx。
type failingCommitter struct{ err error }

func (f failingCommitter) Commit() error { return f.err }

// recordingCommitter 断言成功路径不会凭空造错。
type recordingCommitter struct{ called bool }

func (c *recordingCommitter) Commit() error {
	c.called = true
	return nil
}

// TestCommitBulkTxWrapsCommitFailure：提交失败必须**可被 errors.Is 识别**。
// 这是 httpapi 区分「确定回滚」与「提交二义」的唯一依据；把 %w 改成 %v
// 会让提交二义的批次被误当作回滚，从而删掉已提交账号的临时口令标记。
func TestCommitBulkTxWrapsCommitFailure(t *testing.T) {
	root := errors.New("connection reset during commit")
	err := commitBulkTx(failingCommitter{err: root})
	if err == nil {
		t.Fatal("commitBulkTx returned nil for a failing commit")
	}
	if !errors.Is(err, ErrBulkCommitFailed) {
		t.Fatalf("errors.Is(err, ErrBulkCommitFailed) = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), root.Error()) {
		t.Fatalf("wrapped error loses the underlying cause: %v", err)
	}
	// 底层错误是格式化进去的，不能被 errors.Is 当成其他业务哨兵。
	for _, unrelated := range []error{
		ErrBulkDuplicateUsername, ErrBulkInvalidGroup, ErrBulkSequenceExhausted,
	} {
		if errors.Is(err, unrelated) {
			t.Fatalf("commit failure must not match %v", unrelated)
		}
	}
}

// TestCommitBulkTxReturnsNilOnSuccess：成功提交不上报错误，且确实调用了
// Commit——否则「包装」可能被写成永不真正提交的空壳。
func TestCommitBulkTxReturnsNilOnSuccess(t *testing.T) {
	tx := &recordingCommitter{}
	if err := commitBulkTx(tx); err != nil {
		t.Fatalf("commitBulkTx = %v, want nil", err)
	}
	if !tx.called {
		t.Fatal("commitBulkTx did not call Commit")
	}
}
