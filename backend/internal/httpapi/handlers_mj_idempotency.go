package httpapi

// /mj 提交的完整 Idempotency-Key 编排（W8/MJ-d，R-MJ-06 第二步）。
//
// 顺序与语义：
//
//	解析键 → （限流后）claim → 正常提交 → Complete(首次结果)
//	                              ↘ 任一失败 → Abandon(允许同键重试)
//
// 重放判定以「端点 + 原始请求体」的哈希为准（mj.SubmitRequestHash）：
// 同键同体 → 原样回放首次 {code,description,result}；同键不同体 → 409。
//
// 【与计费幂等键分离】本流程从不读写 request_id / wallet_transactions；
// 计费幂等仍由 billing 引擎的 request_id 承担。把两者混用会让重放请求
// 命中别人的结算会话（relay-00 §5.4 决议 J9）。

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// mjIdempotencyClaim 是一次成功 claim 的请求级状态。
type mjIdempotencyClaim struct {
	UserID int64
	Key    string
	Hash   []byte
	// done 表示「本次请求已产生确定结果」：成功回执已写（或至少任务行已
	// 落库）。done 之后绝不能再 Abandon——删掉已成功的 claim 会让同键重放
	// 变成第二次提交。
	done bool
}

// mjIdempotencyPersistTimeout 是 claim 状态收尾的独立时限。
//
// 客户端可能在收到响应前断开；Complete/Abandon 仍必须落库，否则同键重放
// 会看到 in_flight（409）直到 2 分钟租约过期。用 WithoutCancel 但不无限等。
const mjIdempotencyPersistTimeout = 5 * time.Second

// beginIdempotency 处理可选幂等键并返回是否继续执行。
//
// 返回 (nil,true) 表示请求没有携带幂等键；返回 (claim,true) 表示本次执行
// 者获得执行权；返回 (nil,false) 表示已写出重放/冲突/内部错误响应。
func (h *MJHandler) beginIdempotency(ctx context.Context, w http.ResponseWriter,
	userID int64, key string, endpoint mj.Endpoint, body []byte) (*mjIdempotencyClaim, bool) {

	if key == "" {
		return nil, true
	}
	if h.idempotency == nil {
		// 携带幂等键却无法去重：静默降级会让客户端以为重放安全，实际会
		// 重复建任务/扣费。fail-closed 暴露装配缺陷。
		h.log.Error("mj: idempotency key present but store is not configured",
			"user_id", userID)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError,
			Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return nil, false
	}
	hash := mj.SubmitRequestHash(endpoint, body)
	state, record, err := h.idempotency.ClaimSubmitIdempotency(ctx, tasks.SubmitIdempotencyClaim{
		UserID:      userID,
		Key:         key,
		RequestHash: hash,
		Endpoint:    string(endpoint),
		Now:         h.now(),
	})
	if err != nil {
		h.log.Error("mj: idempotency claim failed", "user_id", userID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError,
			Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return nil, false
	}
	switch state {
	case tasks.SubmitIdempotencyClaimed:
		return &mjIdempotencyClaim{UserID: userID, Key: key, Hash: hash}, true
	case tasks.SubmitIdempotencyReplay:
		if record == nil || len(record.ResponseBody) == 0 {
			// 已完成行必须带响应体（DB CHECK 也保证）；缺失说明数据损坏，
			// 不能凭 public_task_id 猜一个响应给客户端。
			h.log.Error("mj: idempotency replay record is incomplete",
				"user_id", userID, "public_task_id", recordPublicID(record))
			writeMJError(w, &mj.Error{Code: mj.CodeUnknownError,
				Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
			return nil, false
		}
		writeMJReplay(w, record.ResponseBody)
		return nil, false
	case tasks.SubmitIdempotencyConflictBody:
		writeMJError(w, mj.Conflict(mj.DescriptionIdempotencyKeyReused))
		return nil, false
	case tasks.SubmitIdempotencyInFlight:
		writeMJError(w, mj.Conflict(mj.DescriptionIdempotencyInProgress))
		return nil, false
	default:
		h.log.Error("mj: unknown idempotency state", "state", int(state))
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError,
			Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return nil, false
	}
}

func recordPublicID(r *tasks.SubmitIdempotencyRecord) string {
	if r == nil {
		return ""
	}
	return r.PublicTaskID
}

// completeIdempotency 记录首次结果。任务行已落库后才调用。
//
// 无论存储调用是否成功都把 claim 标记 done：任务行已存在、资金已按 hold
// 路径处理，此时删除 claim 会允许同键重放再建一次任务。存储失败留下的
// in_flight 会在租约过期后被接管，接管后的提交受 code 21 同用户复用保护。
func (h *MJHandler) completeIdempotency(ctx context.Context, claim *mjIdempotencyClaim,
	publicTaskID string, envelope mj.Envelope) {

	if claim == nil {
		return
	}
	claim.done = true
	body, err := json.Marshal(envelope)
	if err != nil {
		h.log.Error("mj: marshal idempotency response failed",
			"user_id", claim.UserID, "error", err)
		return
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mjIdempotencyPersistTimeout)
	defer cancel()
	if err := h.idempotency.CompleteSubmitIdempotency(persistCtx, claim.UserID, claim.Key,
		claim.Hash, publicTaskID, body); err != nil {
		h.log.Warn("mj: complete idempotency claim failed",
			"user_id", claim.UserID, "public_task_id", publicTaskID, "error", err)
	}
}

// abandonIdempotency 在请求失败时释放占位（允许同键重试）。
func (h *MJHandler) abandonIdempotency(ctx context.Context, claim *mjIdempotencyClaim) {
	if claim == nil || claim.done || h.idempotency == nil {
		return
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mjIdempotencyPersistTimeout)
	defer cancel()
	if err := h.idempotency.AbandonSubmitIdempotency(persistCtx, claim.UserID, claim.Key, claim.Hash); err != nil {
		h.log.Warn("mj: abandon idempotency claim failed",
			"user_id", claim.UserID, "error", err)
	}
}

// writeMJReplay 原样回放首次响应体（不重新编码：数组/字段顺序保持首次形状）。
func writeMJReplay(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
