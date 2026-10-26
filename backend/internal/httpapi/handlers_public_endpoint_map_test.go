package httpapi

// 公开端点枚举的三方一致性：后端 `publicEndpointMapping`（档案路径 →
// 公开枚举）、前端 `ENDPOINT_PATHS`（枚举 → 档案路径，见
// frontend/src/features/admin/channels/model-profile-wizard-logic.ts）与
// OpenAPI 契约里 `ChannelFetchModelEntry.endpoints` 的 enum。
//
// 共享 fixture `api/fixtures/public-endpoint-map.json` 是前端测试直接深比较的
// 对象（键 = 公开枚举，值 = 档案路径）。这里从反方向核对后端，并断言 fixture
// 的键集与 spec 的 enum 完全一致——任何一侧新增/改名端点而另两侧没同步，
// 这个测试都会变红。

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	publicEndpointFixturePath = "../../../api/fixtures/public-endpoint-map.json"
	adminSchemaPath           = "../../../api/openapi/schemas/admin.yaml"
)

// loadPublicEndpointFixture 读取共享 fixture（枚举 → 路径）。读不到直接失败
// 并打出路径，避免 fixture 被挪走后测试仍然通过。
func loadPublicEndpointFixture(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(publicEndpointFixturePath)
	if err != nil {
		t.Fatalf("读取共享 fixture 失败（路径 %s）: %v", publicEndpointFixturePath, err)
	}
	var fixture map[string]string
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("共享 fixture 不是合法 JSON（路径 %s）: %v", publicEndpointFixturePath, err)
	}
	return fixture
}

// readEndpointEnumFromAdminSchema 从手写的 admin.yaml 里读出
// `ChannelFetchModelEntry.endpoints` 的 enum。
//
// 这里刻意用文本扫描而不是 YAML 解析：httpapi 模块不直接依赖 YAML 库，而
// 该 enum 在源文件里是单行 flow 序列（布局脚本也钉住了这个文件），扫描
// `ChannelFetchModelEntry:` 块内第一条 `enum: [` 既准确又不会引入新依赖。
func readEndpointEnumFromAdminSchema(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 OpenAPI schema 失败（路径 %s）: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "  ChannelFetchModelEntry:") {
			start = index
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s 里找不到 ChannelFetchModelEntry", path)
	}
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		// 下一个同级 schema（两空格缩进）意味着本块结束。
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(trimmed, ":") {
			break
		}
		if !strings.HasPrefix(trimmed, "enum: [") {
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(trimmed, "enum: ["), "]")
		values := make([]string, 0, 8)
		for _, item := range strings.Split(body, ",") {
			if value := strings.TrimSpace(item); value != "" {
				values = append(values, value)
			}
		}
		return values
	}
	t.Fatalf("%s 的 ChannelFetchModelEntry 里找不到 endpoints enum", path)
	return nil
}

func TestPublicEndpointMappingMatchesSharedContract(t *testing.T) {
	fixture := loadPublicEndpointFixture(t)

	// 后端是路径 → 枚举；fixture / 前端是枚举 → 路径。逐条反向后必须完全
	// 相等（键集、值、条数）。
	byEnum := make(map[string]string, len(publicEndpointMapping))
	for _, entry := range publicEndpointMapping {
		if _, exists := byEnum[entry.Value]; exists {
			t.Fatalf("publicEndpointMapping 重复的枚举值 %q", entry.Value)
		}
		byEnum[entry.Value] = entry.Path
	}
	if len(byEnum) != len(fixture) {
		t.Fatalf("条数不一致：后端 %d，fixture %d", len(byEnum), len(fixture))
	}
	if !reflect.DeepEqual(byEnum, fixture) {
		t.Fatalf("fixture 与 publicEndpointMapping 反向不一致：\n后端 %v\nfixture %v", byEnum, fixture)
	}

	// fixture 的键集必须与 OpenAPI spec 的 enum 完全一致（不比较顺序：enum
	// 是契约里的取值集合）。
	enum := readEndpointEnumFromAdminSchema(t, adminSchemaPath)
	if len(enum) != len(fixture) {
		t.Fatalf("enum 条数 %d 与 fixture %d 不一致：%v", len(enum), len(fixture), enum)
	}
	fixtureKeys := make(map[string]struct{}, len(fixture))
	for key := range fixture {
		fixtureKeys[key] = struct{}{}
	}
	enumKeys := make(map[string]struct{}, len(enum))
	for _, value := range enum {
		if _, exists := enumKeys[value]; exists {
			t.Fatalf("spec enum 重复值 %q", value)
		}
		enumKeys[value] = struct{}{}
	}
	if !reflect.DeepEqual(fixtureKeys, enumKeys) {
		t.Fatalf("fixture 键集与 spec enum 不一致：\nfixture %v\nenum %v", fixtureKeys, enumKeys)
	}
}
