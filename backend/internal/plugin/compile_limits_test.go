package plugin

import (
	"context"
	"strings"
	"testing"
	"time"
)

func issuesRule(issues Issues) string {
	for _, issue := range issues {
		if issue.Rule != "" {
			return issue.Rule
		}
	}
	return ""
}

func TestCompileRejectsOversizedSource(t *testing.T) {
	source := strings.Repeat(" ", maxPluginSourceBytes+1)
	_, err := Compile(context.Background(), source, CompileOptions{ExpectedKey: "sample"})
	if err == nil {
		t.Fatal("oversized source must be rejected")
	}
	issues, ok := err.(Issues)
	if !ok {
		t.Fatalf("want Issues, got %T: %v", err, err)
	}
	if issuesRule(issues) != "source_too_large" {
		t.Fatalf("rule = %q, issues = %#v", issuesRule(issues), issues)
	}
}

func TestCompileEnforcesAllocationLimit(t *testing.T) {
	// 顶层分配炸弹：5s 超时抓不到，必须由 TotalAlloc 看门狗中断。
	source := `globalThis.__NEWTG_PLUGIN__ = {};
var bucket = [];
while (true) { bucket.push({x: 1, y: "abcdefgh"}); }`
	start := time.Now()
	_, err := Compile(context.Background(), source, CompileOptions{
		ExpectedKey: "sample",
		Timeout:     3 * time.Second,
		MemoryLimit: 1 << 20,
	})
	if err == nil {
		t.Fatal("allocation bomb must be interrupted")
	}
	issues, ok := err.(Issues)
	if !ok {
		t.Fatalf("want Issues, got %T: %v", err, err)
	}
	if got := issuesRule(issues); got != "memory_limit" {
		t.Fatalf("rule = %q (elapsed %s), issues = %#v", got, time.Since(start), issues)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("allocation guard did not fire promptly: %s", elapsed)
	}
}

func TestCompileEnforcesCallStackLimit(t *testing.T) {
	source := `globalThis.__NEWTG_PLUGIN__ = {};
function recurse() { return recurse(); }
recurse();`
	_, err := Compile(context.Background(), source, CompileOptions{
		ExpectedKey: "sample", Timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("unbounded recursion must fail")
	}
	issues, ok := err.(Issues)
	if !ok {
		t.Fatalf("want Issues, got %T: %v", err, err)
	}
	if got := issuesRule(issues); got != "runtime_error" && got != "timeout" {
		t.Fatalf("unexpected rule %q: %#v", got, issues)
	}
}
