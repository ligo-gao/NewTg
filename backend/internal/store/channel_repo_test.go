package store

// 本文件是 channel_repo 的**纯单元测试**（不连 PostgreSQL）。
//
// 覆盖 M2 final review 的 SQL 形状类修正：
//   - finding 5：jsonb 成员判断必须用 `@>` 包含谓词（jsonb_path_ops GIN 只支持它）；
//   - finding 7：/v1/models 数据源必须包含 model_mapping 的键；
//   - finding 8：批量状态锁必须 ORDER BY id，避免重叠 id 集合死锁。
//
// 真实 SQL 的执行语义由 build tag=integration 的 schema 测试覆盖。

import (
	"strings"
	"testing"
)

func TestCandidatesQueryUsesJSONBContainment(t *testing.T) {
	// finding 5：`supported_models ? $1` / `group_tags ? $2` 无法使用
	// jsonb_path_ops GIN；必须改写为 `@> jsonb_build_array(...)`。
	// 占位符必须显式 ::text：pgx v5 stdlib 不会给 jsonb_build_array 的参数
	// 推断类型，缺省时报 SQLSTATE 42P18（could not determine data type）。
	query, args := candidatesQuery("gpt-4o", "vip")
	if !strings.Contains(query, "supported_models @> jsonb_build_array($1::text)") {
		t.Fatalf("候选查询未使用 supported_models 包含谓词: %s", query)
	}
	if !strings.Contains(query, "(group_tags IS NULL OR group_tags @> jsonb_build_array($2::text))") {
		t.Fatalf("候选查询未使用 group_tags 包含谓词（并保留 NULL 分支）: %s", query)
	}
	if strings.Contains(query, "supported_models ?") || strings.Contains(query, "group_tags ?") {
		t.Fatalf("候选查询仍残留 `?` 存在运算符（无法走 GIN）: %s", query)
	}
	if len(args) != 2 || args[0] != "gpt-4o" || args[1] != "vip" {
		t.Fatalf("候选查询参数 = %v，期望 [gpt-4o vip]", args)
	}

	// 无分组时只保留模型谓词，参数只有一个。
	query, args = candidatesQuery("gpt-4o", "")
	if strings.Contains(query, "group_tags") {
		t.Fatalf("无分组查询不应包含 group_tags 谓词: %s", query)
	}
	if len(args) != 1 {
		t.Fatalf("无分组参数 = %v，期望单参数", args)
	}
}

func TestCandidatesQueryMatchesModelMappingKeys(t *testing.T) {
	// finding 3：/v1/models 宣告 model_mapping 的键，路由候选必须同样接受别名，
	// 否则「列表里有、请求 503」。jsonb_exists 是 `?` 的函数形式（语义相同）。
	query, args := candidatesQuery("alias-model", "vip")
	if !strings.Contains(query, "supported_models @> jsonb_build_array($1::text)") ||
		!strings.Contains(query, "OR jsonb_exists(model_mapping, $1)") {
		t.Fatalf("候选查询必须同时匹配 supported_models 与 model_mapping 键: %s", query)
	}
	if len(args) != 2 || args[0] != "alias-model" || args[1] != "vip" {
		t.Fatalf("候选查询参数 = %v，期望 [alias-model vip]", args)
	}
}

func TestBuildChannelWhereUsesJSONBContainment(t *testing.T) {
	// 列表筛选与路由候选共享同一索引前提（finding 5）；::text 同 Candidates：
	// 防止 pgx stdlib 在 jsonb_build_array 上推断不出参数类型（42P18）。
	where, args := buildChannelWhere(ChannelFilter{Group: "vip", Model: "gpt-4o"})
	if !strings.Contains(where, "group_tags @> jsonb_build_array($1::text)") {
		t.Fatalf("列表筛选缺少 group_tags 包含谓词: %s", where)
	}
	if !strings.Contains(where, "supported_models @> jsonb_build_array($2::text)") {
		t.Fatalf("列表筛选缺少 supported_models 包含谓词: %s", where)
	}
	if strings.Contains(where, "group_tags ?") || strings.Contains(where, "supported_models ?") {
		t.Fatalf("列表筛选仍残留 `?` 存在运算符: %s", where)
	}
	if len(args) != 2 || args[0] != "vip" || args[1] != "gpt-4o" {
		t.Fatalf("列表筛选参数 = %v，期望 [vip gpt-4o]", args)
	}
}

func TestBatchStatusLockQueryOrdersByID(t *testing.T) {
	// finding 8：固定升序的加锁顺序是重叠 id 集合不死锁的前提。
	if !strings.Contains(batchStatusLockQuery, "FOR UPDATE") {
		t.Fatalf("批量锁查询缺少 FOR UPDATE: %s", batchStatusLockQuery)
	}
	if !strings.Contains(batchStatusLockQuery, "ORDER BY id") {
		t.Fatalf("批量锁查询必须 ORDER BY id（死锁防护）: %s", batchStatusLockQuery)
	}
	// ORDER BY 必须在 FOR UPDATE 之前（PostgreSQL 语法：locking clause 在末尾）。
	if strings.Index(batchStatusLockQuery, "ORDER BY id") > strings.Index(batchStatusLockQuery, "FOR UPDATE") {
		t.Fatalf("ORDER BY 必须位于 FOR UPDATE 之前: %s", batchStatusLockQuery)
	}
}

func TestEnabledModelsQueryIncludesModelMappingKeys(t *testing.T) {
	// finding 7 / modules-06 F-06-10：/v1/models 必须同时下发 supported_models
	// 与 model_mapping 的键（客户端可见别名）。
	if !strings.Contains(enabledModelsQuery, "jsonb_array_elements_text(supported_models)") {
		t.Fatalf("模型列表查询缺少 supported_models 来源: %s", enabledModelsQuery)
	}
	if !strings.Contains(enabledModelsQuery, "jsonb_object_keys(model_mapping)") {
		t.Fatalf("模型列表查询缺少 model_mapping 键来源: %s", enabledModelsQuery)
	}
	if !strings.Contains(enabledModelsQuery, "status = 1") {
		t.Fatalf("模型列表查询必须只包含启用渠道: %s", enabledModelsQuery)
	}
}

func TestCountRoutableChannelsQueryShape(t *testing.T) {
	// 渠道数是性能页 models 表的 channels 列：模型全集必须与
	// enabledModelsQuery 同口径（supported_models 成员 + model_mapping 键），
	// 且按渠道去重——同一模型既在 supported_models 又是映射键时只算一个渠道。
	for _, fragment := range []string{
		"CROSS JOIN LATERAL",
		"jsonb_array_elements_text(c.supported_models)",
		"jsonb_object_keys(c.model_mapping)",
		"count(DISTINCT c.id)",
		"WHERE c.status = 1",
		"GROUP BY m.model",
	} {
		if !strings.Contains(countRoutableChannelsQuery, fragment) {
			t.Fatalf("渠道数查询缺少片段 %q: %s", fragment, countRoutableChannelsQuery)
		}
	}
	// 映射的值（上游真实模型名）不是对外可见模型，不得进入统计。
	if strings.Contains(countRoutableChannelsQuery, "jsonb_each_text") {
		t.Fatalf("渠道数查询必须只用 model_mapping 的键: %s", countRoutableChannelsQuery)
	}
}
