package dto

import (
	"os"
	"strings"
	"testing"
)

// 本文件是契约回归的**逐路径**护栏：生成物漂移检查只能证明「源与生成物一致」，
// 证明不了「源本身没有丢响应」——批量编辑时误删某个 GET 的 404 不会被 drift
// 抓到（源与 bundle 一起错）。这里直接对源文件断言。
//
// 只做最小文本扫描，不引入 YAML 依赖：路径块按缩进切分，GET 操作的响应段
// 以第一个同级操作为界。
func TestAdminSpecKeepsDocumentedNotFoundResponses(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/openapi/paths/admin.yaml")
	if err != nil {
		t.Fatalf("read admin.yaml: %v", err)
	}
	spec := string(raw)

	for _, path := range []string{
		"/api/models/{id}",
		"/api/vendors/{id}",
		"/api/subscription-plans/{id}",
		"/api/announcements/{id}",
	} {
		block, ok := pathBlock(spec, path)
		if !ok {
			t.Fatalf("spec is missing path %s", path)
		}
		getOp, ok := operation(block, "get")
		if !ok {
			t.Fatalf("%s no longer declares a GET operation", path)
		}
		if !strings.Contains(getOp, "'404':") {
			t.Fatalf("%s GET must keep its 404 response (regression guard)", path)
		}
	}
}

// pathBlock 返回 `  <path>:` 起、到下一个同级路径（两空格缩进的 `/`）为止的文本。
func pathBlock(spec, path string) (string, bool) {
	key := "  " + path + ":\n"
	start := strings.Index(spec, key)
	if start < 0 {
		return "", false
	}
	rest := spec[start+len(key):]
	if next := strings.Index(rest, "\n  /"); next >= 0 {
		return rest[:next], true
	}
	return rest, true
}

// operation 返回路径块内某个操作的文本（到下一个四空格缩进的方法名为止）。
func operation(block, method string) (string, bool) {
	head := "    " + method + ":\n"
	start := strings.Index(block, head)
	if start < 0 {
		return "", false
	}
	rest := block[start+len(head):]
	for _, next := range []string{"\n    get:", "\n    put:", "\n    post:", "\n    patch:", "\n    delete:"} {
		if idx := strings.Index(rest, next); idx >= 0 {
			return rest[:idx], true
		}
	}
	return rest, true
}
