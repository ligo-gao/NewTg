package plugin

import (
	"context"
	"strings"
	"testing"
)

func TestUtilsJSONClone(t *testing.T) {
	source := fixture{Exports: map[string]string{
		"buildSubmitRequest": `function (ctx) {
    var copy = utils.json.clone(ctx.payload);
    copy.model = "mutated";
    return { original: ctx.payload.model, copy: copy.model, body: copy };
  }`,
	}}.Source()
	plugin := mustCompile(t, source, CompileOptions{})
	value, err := plugin.Call(context.Background(), "buildSubmitRequest", map[string]any{
		"payload": map[string]any{"model": "sample-model"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	out := value.(map[string]any)
	if out["original"] != "sample-model" || out["copy"] != "mutated" {
		t.Fatalf("clone 必须是独立快照: %+v", out)
	}
}

func TestUtilsJSONCloneLimits(t *testing.T) {
	source := fixture{Exports: map[string]string{
		"buildSubmitRequest": `function (ctx) { return utils.json.clone(ctx.payload); }`,
	}}.Source()
	plugin := mustCompile(t, source, CompileOptions{})

	// 33 层嵌套超过 32 层上限。
	var deep any = "leaf"
	for idx := 0; idx < 33; idx++ {
		deep = map[string]any{"n": deep}
	}
	_, err := plugin.Call(context.Background(), "buildSubmitRequest", map[string]any{"payload": deep})
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("runtime_error") || !strings.Contains(issues[0].Message, "层") {
		t.Fatalf("超过层数上限应抛异常: %v", err)
	}

	// 超过 1 MiB 上限。
	big := strings.Repeat("x", (1<<20)+16)
	_, err = plugin.Call(context.Background(), "buildSubmitRequest", map[string]any{"payload": big})
	issues, ok = err.(Issues)
	if !ok || !issues.hasRule("runtime_error") || !strings.Contains(issues[0].Message, "字节") {
		t.Fatalf("超过字节上限应抛异常: %v", err)
	}
}
