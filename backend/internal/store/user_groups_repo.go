package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 本文件提供「用户可用分组 × 分组可用模型」的只读查询，供两处消费：
//
//  1. GET /api/status 的 usable_groups（modules-02 §2.14 / M6c Task 13）；
//  2. PUT /api/self/setting 的 default_model 校验，以及 /pg/* 会话中继的
//     模型准入（relay-01 §1.5：模型不在用户可用分组 → 403、不计费）。
//
// 口径是冻结的（Task 13 brief §2）：
//   - 可用分组 = user_groups.status=1 且（user_selectable=true 或 name = 用户当前分组）；
//   - 分组的 models = 启用渠道（status=1、group_tags NULL 或包含该分组）的
//     supported_models 之并集，且仅保留 model_pricing.status=1 的模型；
//   - 去重、按模型名排序。
//
// 为什么只取 supported_models 而不并入 model_mapping 的键：/v1/models 的对外
// 清单包含映射键（别名），但默认模型/Playground 选择器展示的是「可定价的
// 真实模型」；映射键没有 model_pricing 行，混入会让用户选到一个没有价格、
// 只能靠默认价兜底的别名。两端口径因此而异是有意的。

// UsableGroup 是一个可用分组及其可调用模型清单。
type UsableGroup struct {
	Name        string
	Description string
	Ratio       int32
	// Models 已去重并按字典序排序；无可用模型的启用分组仍出现在结果中
	// （Models 为空切片），让前端能展示「当前分组无可用模型」空态。
	Models []string
}

// UserGroupsRepo 是 user_groups/users/channels/model_pricing 的只读拼接查询。
// 与其它存储类型一样只持有 *sql.DB，不缓存：分组/渠道/定价变更必须即时
// 反映到模型选择器与 default_model 校验。
type UserGroupsRepo struct{ db *sql.DB }

// NewUserGroupsRepo 构造只读分组目录仓储。
func NewUserGroupsRepo(db *sql.DB) *UserGroupsRepo { return &UserGroupsRepo{db: db} }

// UserGroup 返回启用用户的当前分组（users."group"）。
//
// 只读 status='enabled'：被封禁用户的会话可能仍能解析（Redis 会话未吊销），
// 但不应再获得任何模型准入判断的依据；与 sqlPublicUserGroupLookup 同口径。
// 用户行不存在/已停用返回 ErrUserNotFound（调用方按身份失效处理）。
func (r *UserGroupsRepo) UserGroup(ctx context.Context, userID int64) (string, error) {
	if r == nil || r.db == nil {
		return "", ErrUserNotFound
	}
	var group string
	err := r.db.QueryRowContext(ctx,
		`SELECT "group" FROM users WHERE id = $1 AND status = 'enabled'`, userID).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read user group: %w", err)
	}
	return group, nil
}

// usableGroupsQuery 把「可用分组」与「分组可用模型」一次查出：
// 每个 (group, model) 一行，LEFT JOIN 保证无模型的分组也返回。
//
// 说明（索引/规模）：分组侧走 user_groups_status_idx；渠道侧 supported_models
// 的包含判断走 channels_supported_models_gin（jsonb_path_ops 支持 @>）；
// group_tags IS NULL 分支不可索引，但分组数极少（单站点通常 <10），
// 因此不额外建索引。模型清单没有人为上限：它由管理员配置的渠道模型集合
// 决定，通常为几十到几百条，属于前端一次拉取的合理体量。
const usableGroupsQuery = `
	WITH cur AS (
		SELECT "group" AS name FROM users WHERE id = $1 AND status = 'enabled'
	), grp AS (
		SELECT ug.name, COALESCE(ug.description, '') AS description, ug.ratio
		FROM user_groups ug
		CROSS JOIN cur
		WHERE ug.status = 1
		  AND (ug.user_selectable OR ug.name = cur.name)
	)
	SELECT g.name, g.description, g.ratio, m.model
	FROM grp g
	LEFT JOIN LATERAL (
		SELECT DISTINCT e.model
		FROM channels c
		CROSS JOIN LATERAL jsonb_array_elements_text(c.supported_models) AS e(model)
		JOIN model_pricing mp ON mp.model = e.model AND mp.status = 1
		WHERE c.status = 1
		  AND (c.group_tags IS NULL OR c.group_tags @> jsonb_build_array(g.name))
		ORDER BY e.model
	) m ON true
	ORDER BY g.name, m.model`

// UsableGroupsForUser 返回用户的可用分组及每组已定价、有启用渠道的模型清单。
//
// 用户不存在时返回空切片而不是错误：GET /api/status 对匿名/失效会话必须
// 恒 200（公开面），空清单即可；需要区分身份失效的调用方（如 /pg）会先由
// UserGroup 得到 ErrUserNotFound。
func (r *UserGroupsRepo) UsableGroupsForUser(ctx context.Context, userID int64) ([]UsableGroup, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, usableGroupsQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("store: usable groups: %w", err)
	}
	defer rows.Close()

	groups := make([]UsableGroup, 0, 4)
	var last string
	for rows.Next() {
		var (
			name        string
			description string
			ratio       int32
			model       sql.NullString
		)
		if err := rows.Scan(&name, &description, &ratio, &model); err != nil {
			return nil, fmt.Errorf("store: usable groups scan: %w", err)
		}
		if len(groups) == 0 || name != last {
			groups = append(groups, UsableGroup{Name: name, Description: description, Ratio: ratio})
			last = name
		}
		if model.Valid && model.String != "" {
			groups[len(groups)-1].Models = append(groups[len(groups)-1].Models, model.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usable groups iterate: %w", err)
	}
	return groups, nil
}
