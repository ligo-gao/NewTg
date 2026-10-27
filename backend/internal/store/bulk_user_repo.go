package store

// 批量建用户仓储（plan 2026-10-26 Task 9）。
//
// 【为什么必须显式写 must_change_password】登录路径（httpapi/handlers_auth.go）
// 在 users.must_change_password=true 时还要查 Redis 的「临时口令有效」标记；
// 标记缺失直接 401 temp_password_expired。批量 INSERT 若沿用单条创建的列清单
// （不含该列），永远只能批量创建「免改密」账号；反过来，只有 DB 列而漏写
// Redis 标记就会造出永远登录不上的账号。因此本仓储把该列作为显式参数写入，
// 标记的写入时机交给调用方在 afterInsert（提交前）完成，二者必须成对。
//
// 【顺序分配为什么用 advisory lock + bigint】生成端点需要 NewTG_user0001 这类
// 连续用户名。MAX(substring(...)::int) 会被普通用户注册的
// NewTG_user999999999999999999999 打爆（int 溢出 → 生成端点永久 500），
// 所以正则收紧到 1–9 位数字、强制转换用 bigint，并在进入 INSERT 前对
// seq+count 做上限判断。advisory lock 只序列化本端点；用户自行注册同名账号
// 的竞争窗口由唯一键兜底（整批 409，不会脏写）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	// bulkUserSequenceMax 是 NewTG_user 后缀允许的最大值（9 位十进制）。
	// 达到上限后生成端点返回明确错误，而不是让 INSERT 失败或整数溢出。
	bulkUserSequenceMax = int64(999_999_999)
)

// 批量建用户的哨兵错误，由 httpapi 映射为 409/422。
var (
	// ErrBulkDuplicateUsername：批内任一用户名与库内已有账号冲突。
	ErrBulkDuplicateUsername = errors.New("store: bulk user username already exists")
	// ErrBulkInvalidGroup：批内任一用户引用了不存在的分组。
	ErrBulkInvalidGroup = errors.New("store: bulk user group does not exist")
	// ErrBulkSequenceExhausted：NewTG_user 序号已接近 9 位上限，无法再分配。
	ErrBulkSequenceExhausted = errors.New("store: bulk user sequence exhausted")
	// ErrBulkCommitFailed：事务 COMMIT 失败。提交是**模棱两可**的——连接在
	// COMMIT 期间断开时，服务端可能已经提交了。调用方不得据此删除 afterInsert
	// 写入的临时口令标记：保留标记在「已提交」与「未提交」两种结局下都安全
	// （未提交的标记只是孤立键；身份 id 永不复用，绝不会附着到以后的账号），
	// 而删除标记一旦实际已提交，就会造出永远 401 temp_password_expired 的账号
	// （spec §11.16）。
	ErrBulkCommitFailed = errors.New("store: bulk user commit failed")
)

// BulkUserRow 是一行待插入的批量用户。PasswordHash 已是 argon2id PHC 串；
// 明文口令绝不进入仓储层。
type BulkUserRow struct {
	Username     string
	PasswordHash string
	CustomerType string
	Group        string
}

// BulkCreateOptions 控制批量插入的两个可选项。
type BulkCreateOptions struct {
	// MustChangePassword 写入 users.must_change_password；调用方必须同时在
	// afterInsert 里写无 TTL 的临时口令标记。
	MustChangePassword bool
	// AssignSequential 在事务内取 advisory lock 并按 NewTG_user%04d 重写
	// rows[i].Username（从当前最大序号 +1 开始）。
	AssignSequential bool
}

// BulkUserRepo 是批量建用户的 PostgreSQL 实现。
type BulkUserRepo struct {
	db *sql.DB
}

// NewBulkUserRepo 构造批量建用户仓储。
func NewBulkUserRepo(db *sql.DB) *BulkUserRepo {
	return &BulkUserRepo{db: db}
}

// ExistingUsernames 返回 usernames 中已存在于 users 表的集合。
func (r *BulkUserRepo) ExistingUsernames(ctx context.Context, usernames []string) (map[string]bool, error) {
	out := make(map[string]bool, len(usernames))
	if len(usernames) == 0 {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT username FROM users WHERE username = ANY($1::text[])`, usernames)
	if err != nil {
		return nil, fmt.Errorf("store: check existing usernames: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan existing username: %w", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate existing usernames: %w", err)
	}
	return out, nil
}

// ExistingGroups 返回 names 中已存在于 user_groups 表的集合。
func (r *BulkUserRepo) ExistingGroups(ctx context.Context, names []string) (map[string]bool, error) {
	out := make(map[string]bool, len(names))
	if len(names) == 0 {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT name FROM user_groups WHERE name = ANY($1::text[])`, names)
	if err != nil {
		return nil, fmt.Errorf("store: check existing groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan existing group: %w", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate existing groups: %w", err)
	}
	return out, nil
}

// BulkCreateUsers 在一个事务内插入全部行（全有或全无）。
//
// afterInsert 在 INSERT 之后、Commit 之前调用，用于写临时口令的 Redis 标记；
// 它返回错误时事务确定回滚，调用方负责清理可能已写入的标记。Commit 失败时
// 返回包装了 ErrBulkCommitFailed 的错误：提交结果不确定，调用方**不得**按
// afterInsert 记录的 ids 删除标记（保留是两种结局下都安全的选择，见
// ErrBulkCommitFailed 的注释）。
//
// 返回的是重写后的 rows（AssignSequential 只改 Username，不改顺序），生成
// 端点据此把明文口令与用户名按输入顺序配对。
func (r *BulkUserRepo) BulkCreateUsers(
	ctx context.Context,
	rows []BulkUserRow,
	opts BulkCreateOptions,
	afterInsert func(tx *sql.Tx, ids []int64) error,
) ([]BulkUserRow, error) {
	if len(rows) == 0 {
		return rows, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin bulk user tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if opts.AssignSequential {
		// 事务级 advisory lock：并发生成请求串行分配序号，提交/回滚自动释放。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('newtg.bulk_user_seq'))`); err != nil {
			return nil, fmt.Errorf("store: lock bulk user sequence: %w", err)
		}
		var current int64
		// 正则限定 1–9 位数字、强制转换用 bigint：任何用户自行注册的超长
		// NewTG_user… 名字都不会让这条查询报错（修正 5）。
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(substring(username FROM 11)::bigint), 0)
			FROM users
			WHERE username ~ '^NewTG_user[0-9]{1,9}$'`).Scan(&current); err != nil {
			return nil, fmt.Errorf("store: read bulk user sequence: %w", err)
		}
		if current+int64(len(rows)) > bulkUserSequenceMax {
			return nil, ErrBulkSequenceExhausted
		}
		for i := range rows {
			rows[i].Username = fmt.Sprintf("NewTG_user%04d", current+int64(i)+1)
		}
	}

	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		var id int64
		// must_change_password 必须显式写入（修正 1）：列清单与单条创建不同。
		err := tx.QueryRowContext(ctx, `
			INSERT INTO users (username, customer_type, "group", password_hash, must_change_password, role, status)
			VALUES ($1, $2, $3, $4, $5, 'user', 'enabled')
			RETURNING id`,
			row.Username, row.CustomerType, row.Group, row.PasswordHash, opts.MustChangePassword,
		).Scan(&id)
		if err != nil {
			switch {
			case isUniqueViolation(err):
				return nil, ErrBulkDuplicateUsername
			case isForeignKeyViolation(err):
				return nil, ErrBulkInvalidGroup
			}
			return nil, fmt.Errorf("store: bulk insert user %q: %w", row.Username, err)
		}
		ids = append(ids, id)
	}

	if afterInsert != nil {
		if err := afterInsert(tx, ids); err != nil {
			return nil, err
		}
	}
	if err := commitBulkTx(tx); err != nil {
		return nil, err
	}
	return rows, nil
}

// bulkTxCommitter 是 *sql.Tx 的最小提交接口。抽成接口只有一个目的：让单测
// 能不连 PostgreSQL 就钉住 ErrBulkCommitFailed 的包装方式（见
// bulk_user_repo_test.go）——COMMIT 失败本身的路径无法在真实数据库上可靠触发。
type bulkTxCommitter interface {
	Commit() error
}

// commitBulkTx 把 COMMIT 失败包装为 ErrBulkCommitFailed。
//
// 必须用 %w：调用方（httpapi 的 cleanupMarkersAfterFailure）靠 errors.Is
// 区分「确定回滚」与「提交二义」。改成 %v 会让二义路径被当成确定失败，
// 进而删掉 already-committed 账号的临时口令标记，造出永远 401
// temp_password_expired 的账号（spec §11.16）。底层错误走 %v，因此它不会
// 被 errors.Is 误命中。
func commitBulkTx(tx bulkTxCommitter) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %v", ErrBulkCommitFailed, err)
	}
	return nil
}
