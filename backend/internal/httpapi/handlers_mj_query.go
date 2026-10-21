// Package httpapi 的绘图任务族查询端点（W8/MJ-b）。
//
// 端点面（relay-05 §1.2）：
//
//	GET  /mj/task/{id}/fetch             单任务查询（只读本网关任务，不访问上游）
//	POST /mj/task/list-by-condition      批量查询（ids 空数组 → []）
//	POST /mj/task/{id}/image-seed        锁定原任务渠道转发 image-seed（30s）
//
// 【归属纪律】fetch/list/image-seed 全部按 `(user_id, MjId)` 读取任务：
// 他人任务与不存在任务返回同一个 404 + 同一个固定文案，不制造存在性 oracle
// （R-MJ-05）。图片代理（MJ-c）的签名 URL 不在本文件范围。
//
// 【为什么不访问上游做 fetch】设计 §1.2/§8：客户端轮询网关、网关后台轮询
// 上游（两级轮询）。fetch 直接投影本网关已回填的任务行；若客户端查询也能
// 触达上游，N 个客户端会变成 N 倍上游负载，并绕过后台轮询的单副本/CAS 收敛。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// mjTaskNotFound 是查询族「不存在/他人任务」的统一出口。
//
// 404 + code 4：MJ 客户端按 code 分支；description 固定，绝不携带 MjId 或
// 归属信息（否则响应本身就成了枚举 oracle）。
func mjTaskNotFound() *mj.Error {
	return &mj.Error{
		Code: mj.CodeRequestError, Description: mj.DescriptionTaskNotFound,
		Status: http.StatusNotFound,
	}
}

// handleTaskFetch 处理 GET /mj/task/{id}/fetch。
func (h *MJHandler) handleTaskFetch(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInvalidToken, Status: http.StatusUnauthorized})
		return
	}
	id, mjErr := mj.ParseMjID(r.PathValue("id"))
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	task, err := h.store.GetByUserUpstreamID(r.Context(), principal.UserID, id)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeMJError(w, mjTaskNotFound())
			return
		}
		h.log.Error("mj: read task for fetch failed", "user_id", principal.UserID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return
	}
	writeMJJSON(w, mj.NewSuccess("", mj.TaskDTO(task)))
}

// mjListRequest 是 list-by-condition 的请求体（基线内联结构 `{ids:[...]}`）。
type mjListRequest struct {
	IDs []string `json:"ids"`
}

// handleTaskList 处理 POST /mj/task/list-by-condition。
//
// 批量查询只返回**本人且存在**的任务：不存在的 id 直接跳过（批量语义没有
// 单条错误通道；逐 id 报错会泄露「哪些 MjId 存在」）。结果按输入顺序排列、
// 保留重复项（去重会改变客户端按输入对位的语义）。
func (h *MJHandler) handleTaskList(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInvalidToken, Status: http.StatusUnauthorized})
		return
	}
	if r.Body == nil {
		writeMJError(w, mj.BadRequest(mj.DescriptionInvalidRequest))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMJListBodyBytes)
	var req mjListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 空 body / 畸形 JSON 都是客户端构造错误；不区分具体原因。
		writeMJError(w, mj.BadRequest(mj.DescriptionInvalidRequest))
		return
	}
	ids, mjErr := mj.ParseListIDs(req.IDs)
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	// 空数组 → []，不查库（设计 §2.2 的显式语义）。
	if len(ids) == 0 {
		writeMJJSON(w, mj.NewSuccess("", []any{}))
		return
	}
	rows, err := h.store.ListByUserUpstreamIDs(r.Context(), principal.UserID, ids, mj.MaxListIDs)
	if err != nil {
		h.log.Error("mj: batch read tasks failed", "user_id", principal.UserID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return
	}
	byID := make(map[string]*tasks.Task, len(rows))
	for _, row := range rows {
		byID[row.UpstreamTaskID] = row
	}
	results := make([]any, 0, len(ids))
	for _, id := range ids {
		if row, found := byID[id]; found {
			results = append(results, mj.TaskDTO(row))
		}
	}
	writeMJJSON(w, mj.NewSuccess("", results))
}

// handleImageSeed 处理 POST /mj/task/{id}/image-seed。
//
// 按原任务所属渠道重建子请求（relay-05 §1.2/§4.1）：先按归属取任务，再用
// 原渠道 baseURL/凭据转发，超时 30s。渠道被禁用时拒绝（与动作类的锁定
// 三校验同口径），不回落其它渠道——seed 是原任务的结果，换渠道没有意义。
func (h *MJHandler) handleImageSeed(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInvalidToken, Status: http.StatusUnauthorized})
		return
	}
	id, mjErr := mj.ParseMjID(r.PathValue("id"))
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	task, err := h.store.GetByUserUpstreamID(r.Context(), principal.UserID, id)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeMJError(w, mjTaskNotFound())
			return
		}
		h.log.Error("mj: read task for image-seed failed", "user_id", principal.UserID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return
	}
	channelID := task.LockedChannelID
	if channelID == 0 {
		channelID = task.ChannelID
	}
	if channelID <= 0 {
		writeMJError(w, mj.BadRequest(mj.DescriptionOriginTaskMissing))
		return
	}
	channel, err := h.channels.GetChannel(r.Context(), channelID)
	if err != nil {
		if errors.Is(err, tasks.ErrChannelNotFound) {
			writeMJError(w, mj.BadRequest(mj.DescriptionOriginTaskMissing))
			return
		}
		h.log.Error("mj: read origin channel for image-seed failed", "channel_id", channelID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return
	}
	if channel.Status != mj.ChannelStatusEnabled {
		writeMJError(w, mj.BadRequest(mj.DescriptionChannelDisabled))
		return
	}
	if channel.BaseURL == "" {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionChannelMisconfigured, Status: http.StatusInternalServerError})
		return
	}
	apiKey, err := h.decryptChannelCredential(channel.Credentials)
	if err != nil {
		h.log.Error("mj: channel credential is not usable for image-seed", "channel_id", channel.ID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionChannelMisconfigured, Status: http.StatusInternalServerError})
		return
	}
	body, mjErr := h.querier.ImageSeed(r.Context(), mj.SubmitConfig{
		BaseURL: channel.BaseURL,
		APIKey:  apiKey,
	}, id)
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	// 上游 seed 查询体透传（设计 §1.2）：已是 JSON 信封，不改写。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
