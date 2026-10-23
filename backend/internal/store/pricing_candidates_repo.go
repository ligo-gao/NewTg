package store

// 已添加模型的候选并集（模型定价策略 spec §5.4 / 计划 Task 5）。
//
// 【为什么不是 ListPricingModels】那个端点只查 model_pricing 已有行，正是
// 「渠道里加了模型、定价页却选不到」的原因。这里把四个来源合并后去重：
//   - model_meta.model_name（catalog）；
//   - channels.supported_models（channel）；
//   - channels.model_mapping 的**键**（channel；客户端可见别名，与
//     /v1/models 的并集口径一致，见 channel_repo.go 的 enabledModelsQuery）；
//   - model_pricing.model（pricing）。
//
// 【为什么不返回渠道字段】候选表只服务「选一个模型来定价」，渠道凭据、
// 渠道 id、上游地址与路由细节都不属于这个响应面；一旦下发就会把内部拓扑
// 泄漏到管理端以外的渲染层（浏览器 devtools / 前端日志）。
//
// 【priced 与 status】priced 只表示「存在定价行」，停用行也算已定价——
// 选择器要展示的是「这个模型有没有被配置过」，重新启用比新建更符合预期。
// 未定价行的 mode 固定 per_token，作为新建草稿的默认计费方式。

import (
	"context"
	"fmt"
	"strings"
)

// 候选来源枚举。与公开/管理契约里的 sources 数组元素一一对应。
const (
	PricingCandidateSourceCatalog = "catalog"
	PricingCandidateSourceChannel = "channel"
	PricingCandidateSourcePricing = "pricing"
)

// PricingCandidate 是模型选择器的一行。
type PricingCandidate struct {
	Model string
	// DisplayName/Vendor/ContextLength 来自目录档案；缺失时保持 nil，由前端
	// 回退到 model 名与空值，不在这里编造展示字段。
	DisplayName   *string
	Vendor        *string
	ContextLength *int64
	// Sources 是去重后的来源集合，顺序固定为 catalog → channel → pricing。
	Sources []string
	Priced  bool
	// Mode 取 per_token / per_request / tiered_expr；未定价行为 per_token。
	Mode string
}

// PricingCandidateFilter 是候选查询条件。
type PricingCandidateFilter struct {
	// Keyword 是大小写不敏感的模型名子串；空串表示不筛选。
	Keyword string
	Limit   int
}

// ListPricingCandidates 实现候选并集查询。
//
// 关键字用 strpos 而不是 LIKE：模型名允许 `%`/`_`（字符集含 ._:/-，不含
// 通配符，但模型映射键来自外部导入），转义规则容易漏；strpos 只做字面
// 子串匹配，语义与「在列表里搜索」一致。子串（而不是前缀）是选择器的
// 使用方式：用户输入 `gpt` 应能找到 `openai/gpt-5`。
func (r *SQLPricingRepo) ListPricingCandidates(ctx context.Context, filter PricingCandidateFilter) ([]PricingCandidate, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	keyword := strings.TrimSpace(filter.Keyword)

	rows, err := r.db.QueryContext(ctx, `
		WITH channel_models AS (
			SELECT DISTINCT m AS model
			FROM (
				SELECT jsonb_array_elements_text(supported_models) AS m
				FROM channels
				UNION ALL
				SELECT jsonb_object_keys(model_mapping) AS m
				FROM channels
			) AS cm
			WHERE m <> ''
		),
		model_names AS (
			SELECT model_name AS model FROM model_meta
			UNION
			SELECT model FROM channel_models
			UNION
			SELECT model FROM model_pricing
		)
		SELECT n.model,
		       m.display_name,
		       COALESCE(NULLIF(v.name, ''), NULLIF(mp.vendor, '')) AS vendor,
		       m.context_length,
		       (m.model_name IS NOT NULL) AS in_catalog,
		       (cm.model IS NOT NULL) AS in_channel,
		       (mp.model IS NOT NULL) AS priced,
		       CASE
		           WHEN mp.model IS NULL THEN 'per_token'
		           WHEN mp.billing_expr IS NOT NULL THEN 'tiered_expr'
		           WHEN mp.mode = 1 THEN 'per_request'
		           ELSE 'per_token'
		       END AS mode
		FROM model_names n
		LEFT JOIN model_meta m ON m.model_name = n.model
		LEFT JOIN vendors v ON v.id = m.vendor_id
		LEFT JOIN model_pricing mp ON mp.model = n.model
		LEFT JOIN channel_models cm ON cm.model = n.model
		WHERE n.model <> ''
		  AND ($1 = '' OR strpos(lower(n.model), lower($1)) > 0)
		ORDER BY n.model ASC
		LIMIT $2`, keyword, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pricing candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]PricingCandidate, 0, limit)
	for rows.Next() {
		var (
			candidate            PricingCandidate
			inCatalog, inChannel bool
			priced               bool
			mode                 string
		)
		if err := rows.Scan(
			&candidate.Model, &candidate.DisplayName, &candidate.Vendor,
			&candidate.ContextLength, &inCatalog, &inChannel, &priced, &mode,
		); err != nil {
			return nil, fmt.Errorf("store: scan pricing candidate: %w", err)
		}
		candidate.Priced = priced
		candidate.Mode = mode
		candidate.Sources = make([]string, 0, 3)
		// 顺序固定，避免 map 迭代导致响应在两次请求间抖动（前端做 diff 时
		// 会误判为变化）。
		if inCatalog {
			candidate.Sources = append(candidate.Sources, PricingCandidateSourceCatalog)
		}
		if inChannel {
			candidate.Sources = append(candidate.Sources, PricingCandidateSourceChannel)
		}
		if priced {
			candidate.Sources = append(candidate.Sources, PricingCandidateSourcePricing)
		}
		items = append(items, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pricing candidates: %w", err)
	}
	return items, nil
}
