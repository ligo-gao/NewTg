package httpapi

// 审计写入的公共口径。
//
// 两条约束必须同时成立：
//   1. 日志库独立于业务库，没有跨库事务——审计失败绝不能回滚已完成的业务；
//   2. 日志库慢/不可用时不能把请求拖到连接超时——每次写入以 3 秒为上限。
//
// context.WithoutCancel 保证客户端在审计写出前断开连接时，已经发生的事件
// 仍会被记录（请求取消只应终止业务工作，不应抹掉事实）。

import (
	"context"
	"time"

	"newtg/gateway/internal/audit"
)

// auditWriteTimeout 是单次审计写入的上限（与令牌审计此前的 tokenAuditTimeout
// 同值，统一收敛到一处）。
const auditWriteTimeout = 3 * time.Second

// writeAuditEvent 以 best-effort + 有界超时写一条审计。logger 为 nil 时
// 直接跳过（仅测试/未装配日志库的装配）。
func writeAuditEvent(ctx context.Context, logger *audit.Logger, event audit.Event) error {
	if logger == nil {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	return logger.LogEvent(writeCtx, event)
}
