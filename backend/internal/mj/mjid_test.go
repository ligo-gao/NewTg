package mj

import (
	"strings"
	"testing"

	"newtg/gateway/internal/tasks"
)

// TestValidMjID 钉死 R-MJ-16 的 id 字符集与长度边界。
func TestValidMjID(t *testing.T) {
	valid := []string{
		"a",
		"abcDEF0123",
		"abc_def-123",
		strings.Repeat("a", 128),
	}
	for _, id := range valid {
		if !ValidMjID(id) {
			t.Fatalf("%q 应为合法 MjId", id)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", 128+1),
		"a b",
		"a/b",
		"a.b",
		"a:b",
		"a\tb",
		"abc\n",
		"ümlaut",
		"abc%20",
	}
	for _, id := range invalid {
		if ValidMjID(id) {
			t.Fatalf("%q 不应为合法 MjId", id)
		}
	}
}

// TestParseMjID 校验失败必须是固定 invalid_id 400。
func TestParseMjID(t *testing.T) {
	got, err := ParseMjID("abc_123")
	if err != nil || got != "abc_123" {
		t.Fatalf("ParseMjID = (%q,%v)", got, err)
	}
	if _, err := ParseMjID("bad/id"); err == nil ||
		err.Code != CodeRequestError || err.Description != DescriptionInvalidID {
		t.Fatalf("非法 id 应返回 invalid_id，got %#v", err)
	}
	// 不静默 TrimSpace：前后空白是客户端错误，必须原样拒绝。
	if _, err := ParseMjID(" abc "); err == nil {
		t.Fatalf("前后空白不得被静默修剪")
	}
}

// TestReuseKeyAndReusableTask 钉死 R-MJ-05 的同用户复用判定。
func TestReuseKeyAndReusableTask(t *testing.T) {
	if _, err := NewReuseKey(0, "abc"); err == nil ||
		err.Description != DescriptionInvalidRequest {
		t.Fatalf("userID<=0 应返回 invalid_request，got %#v", err)
	}
	if _, err := NewReuseKey(1, "bad/id"); err == nil ||
		err.Description != DescriptionInvalidID {
		t.Fatalf("非法 MjId 应返回 invalid_id，got %#v", err)
	}
	key, err := NewReuseKey(7, "abc")
	if err != nil || key.UserID != 7 || key.UpstreamTaskID != "abc" {
		t.Fatalf("NewReuseKey = (%+v,%v)", key, err)
	}

	// 同用户、同 MjId、family=mj → 复用。
	if !ReusableTask(&tasks.Task{Family: tasks.FamilyMJ, UserID: 7, UpstreamTaskID: "abc"}, key) {
		t.Fatalf("同用户同 MjId 的 MJ 行应可复用")
	}
	// 不同用户 / 不同 MjId / 插件族 / nil 都不得复用。
	for name, existing := range map[string]*tasks.Task{
		"other user":     {Family: tasks.FamilyMJ, UserID: 8, UpstreamTaskID: "abc"},
		"other mj id":    {Family: tasks.FamilyMJ, UserID: 7, UpstreamTaskID: "xyz"},
		"plugin":         {Family: tasks.FamilyPlugin, UserID: 7, UpstreamTaskID: "abc"},
		"empty upstream": {Family: tasks.FamilyMJ, UserID: 7, UpstreamTaskID: ""},
		"nil":            nil,
	} {
		if ReusableTask(existing, key) {
			t.Fatalf("%s 不得复用（跨用户/跨族/空 id 会把任务或退款串味）", name)
		}
	}
	// a2 若绕过 NewReuseKey 直接构造空 key，也不得匹配任何行（含 upload inline）。
	emptyKey := ReuseKey{UserID: 7}
	if ReusableTask(&tasks.Task{Family: tasks.FamilyMJ, UserID: 7, UpstreamTaskID: ""}, emptyKey) {
		t.Fatalf("空 UpstreamTaskID 必须拒绝复用")
	}
}

// TestLimitCandidates 图片代理候选数上限（R-MJ-05）。
func TestLimitCandidates(t *testing.T) {
	small := make([]int, 100)
	if got := LimitCandidates(small); len(got) != len(small) {
		t.Fatalf("小集合不应被截断: %d", len(got))
	}
	large := make([]int, 100+50)
	for i := range large {
		large[i] = i
	}
	got := LimitCandidates(large)
	if len(got) != 100 {
		t.Fatalf("候选数 = %d，期望 %d", len(got), 100)
	}
	for i := range got {
		if got[i] != i {
			t.Fatalf("截断必须保持顺序: got[%d]=%d", i, got[i])
		}
	}
}

// TestParseListIDs 钉死 list-by-condition 的条数与形状上界。
func TestParseListIDs(t *testing.T) {
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = "id" + strings.Repeat("a", i%10)
	}
	if _, err := ParseListIDs(ids); err != nil {
		t.Fatalf("恰好 100 个 id 应通过: %v", err)
	}
	if _, err := ParseListIDs(append(ids, "extra")); err == nil ||
		err.Description != DescriptionTooManyIDs {
		t.Fatalf("101 个 id 应返回 too_many_ids，got %#v", err)
	}
	if _, err := ParseListIDs([]string{"ok", "bad/id"}); err == nil ||
		err.Description != DescriptionInvalidID {
		t.Fatalf("非法 id 应返回 invalid_id，got %#v", err)
	}
	// 空数组合法，且保持顺序/重复（去重会改变客户端按输入对位的语义）。
	got, err := ParseListIDs([]string{})
	if err != nil || len(got) != 0 {
		t.Fatalf("空数组 = (%v,%v)", got, err)
	}
	got, err = ParseListIDs([]string{"b", "a", "b"})
	if err != nil {
		t.Fatalf("ParseListIDs: %v", err)
	}
	if strings.Join(got, ",") != "b,a,b" {
		t.Fatalf("顺序/重复被改变: %v", got)
	}
}
