package auth

// 强制改密（must_change_password）的会话限制标记。
//
// 任务建议按会话记录（pwd_restricted:<sid>）。实现选择按**用户**记录
// （pwd_restricted:<userID>），原因：
//   - must_change_password 是 users 行的属性，不是某次登录的属性；按用户
//     记录让所有会话签发路径（口令登录、2FA challenge、未来新增路径）自动
//     继承限制，不需要每个签发点记得传标志；
//   - Redis 键与用户同寿命（30d 绝对会话上限），不会出现「会话在、标记先
//     过期」的独立失败模式；
//   - 代价是每次 Cookie 鉴权多一次 EXISTS。对低频管理面可接受，且 Redis
//     故障时 SessionAuth 本就 fail-closed（Get/Touch 先失败）。
//
// 生命周期：
//   - 登录成功且 users.must_change_password=true 时写入（TTL = 用户绝对会话
//     上限 30d，每次登录刷新）；
//   - 登录成功且标志为 false 时删除（自愈历史上删除失败的残留标记）；
//   - 用户自选新口令成功（POST /api/password、重置确认）后删除。

import (
	"context"
	"fmt"
	"strconv"
)

// passwordChangeRequiredKeyPrefix 是 Redis 键前缀；后缀是十进制 userID。
const passwordChangeRequiredKeyPrefix = "pwd_restricted:"

func passwordChangeRequiredKey(userID int64) string {
	return passwordChangeRequiredKeyPrefix + strconv.FormatInt(userID, 10)
}

// MarkPasswordChangeRequired 把用户标记为「必须改密」。TTL 取用户档绝对
// 会话上限：标记只需覆盖该用户可能存在的会话寿命，用户改密后立即删除。
func (m *SessionManager) MarkPasswordChangeRequired(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return fmt.Errorf("auth: mark password change required: invalid user id %d", userID)
	}
	if err := m.rdb.Set(ctx, passwordChangeRequiredKey(userID), "1", userAbsoluteTimeout).Err(); err != nil {
		return fmt.Errorf("auth: mark password change required: %w", err)
	}
	return nil
}

// ClearPasswordChangeRequired 删除用户的强制改密标记。幂等；userID 非法时
// 视为无标记（不是错误——调用方可能在已删除用户上做清理）。
func (m *SessionManager) ClearPasswordChangeRequired(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return nil
	}
	if err := m.rdb.Del(ctx, passwordChangeRequiredKey(userID)).Err(); err != nil {
		return fmt.Errorf("auth: clear password change required: %w", err)
	}
	return nil
}

// PasswordChangeRequired 报告用户是否处于强制改密受限态。
// Redis 错误原样上抛：调用方必须 fail-closed（GC-7），不能把「读不到标记」
// 当成「不受限」。
func (m *SessionManager) PasswordChangeRequired(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	n, err := m.rdb.Exists(ctx, passwordChangeRequiredKey(userID)).Result()
	if err != nil {
		return false, fmt.Errorf("auth: read password change required: %w", err)
	}
	return n == 1, nil
}
