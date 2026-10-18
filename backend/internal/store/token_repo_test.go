package store

// 本文件是 token_repo 的**纯单元测试**（不连 PostgreSQL）。
//
// Resolve 的 SQL 由集成测试覆盖；这里固定两条纯解码规则：
//   - api_tokens.model_limits / allow_ips（finding 6）：NULL / jsonb null / [] = 不限；
//   - users.admin_permissions（finding 15）：SQL NULL = 全权，jsonb null = 无权限，
//     两者绝不能被压平。

import (
	"reflect"
	"testing"
)

func TestDecodeJSONStringArray(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want []string
	}{
		{"SQL NULL = 不限", nil, nil},
		{"jsonb null = 不限", []byte("null"), nil},
		{"空数组 = 不限", []byte("[]"), []string{}},
		{"普通数组", []byte(`["gpt-4o","claude-sonnet"]`), []string{"gpt-4o", "claude-sonnet"}},
		{"去空白与空项", []byte(`["  gpt-4o ","", "  "]`), []string{"gpt-4o"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeJSONStringArray(tc.raw)
			if err != nil {
				t.Fatalf("decodeJSONStringArray(%q): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeJSONStringArray(%q) = %#v，期望 %#v", tc.raw, got, tc.want)
			}
		})
	}

	if _, err := decodeJSONStringArray([]byte(`"gpt-4o"`)); err == nil {
		t.Fatal("标量 JSON 应被拒绝（fail-closed），而不是当成白名单")
	}
	if _, err := decodeJSONStringArray([]byte(`[1,2]`)); err == nil {
		t.Fatal("非字符串数组应被拒绝（fail-closed）")
	}
}

func TestDecodeAdminPermissionsDistinguishesNullShapes(t *testing.T) {
	// finding 15：SQL NULL（raw == nil）与 jsonb `null` 必须区分。
	perms, err := decodeAdminPermissions(nil)
	if err != nil {
		t.Fatalf("SQL NULL: %v", err)
	}
	if perms != nil {
		t.Fatalf("SQL NULL 应返回 nil（全权），得到 %#v", perms)
	}

	perms, err = decodeAdminPermissions([]byte("null"))
	if err != nil {
		t.Fatalf("jsonb null: %v", err)
	}
	if perms == nil {
		t.Fatal("jsonb null 必须是**非 nil 空切片**（无权限），不能压平成全权")
	}
	if len(perms) != 0 {
		t.Fatalf("jsonb null = %#v，期望空集合", perms)
	}

	perms, err = decodeAdminPermissions([]byte(`[]`))
	if err != nil {
		t.Fatalf("空数组: %v", err)
	}
	if perms == nil || len(perms) != 0 {
		t.Fatalf("空数组 = %#v，期望非 nil 空集合", perms)
	}

	perms, err = decodeAdminPermissions([]byte(`["*"]`))
	if err != nil {
		t.Fatalf("权限数组: %v", err)
	}
	if !reflect.DeepEqual(perms, []string{"*"}) {
		t.Fatalf("权限数组 = %#v，期望 [\"*\"]", perms)
	}

	if _, err := decodeAdminPermissions([]byte(`{"oops":true}`)); err == nil {
		t.Fatal("非数组形状应返回错误（fail-closed）")
	}
}
