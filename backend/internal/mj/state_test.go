package mj

import (
	"testing"

	"newtg/gateway/internal/tasks"
)

// TestMapStateFrozenTable 是 R-MJ-15 的完整映射表。
//
// 任何一档映射错位都必须让本表变红：把 MODAL 当 unknown、把 CANCEL 当
// unknown、或把未知串猜成 failure，都会改变轮询/退款行为（资金侧）。
func TestMapStateFrozenTable(t *testing.T) {
	cases := []struct {
		raw  string
		want tasks.State
	}{
		{"NOT_START", tasks.StateNotStart},
		{"SUBMITTED", tasks.StateSubmitted},
		{"QUEUED", tasks.StateQueued},
		{"MODAL", tasks.StateInProgress},
		{"IN_PROGRESS", tasks.StateInProgress},
		{"SUCCESS", tasks.StateSuccess},
		{"FAILURE", tasks.StateFailure},
		{"CANCEL", tasks.StateFailure},
		// 大小写/空白归一化只影响匹配，不改变映射。
		{"not_start", tasks.StateNotStart},
		{"  Success  ", tasks.StateSuccess},
		// 其它一律 unknown：不得猜成进行中或失败。
		{"", tasks.StateUnknown},
		{"BOGUS", tasks.StateUnknown},
		{"CANCELLED", tasks.StateUnknown},
	}
	for _, c := range cases {
		if got := MapState(c.raw); got != c.want {
			t.Fatalf("MapState(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

// TestNormalizeStatusPreservesRaw 钉死 raw_state 保真与进度归一。
func TestNormalizeStatusPreservesRaw(t *testing.T) {
	got := NormalizeStatus("  CANCEL  ", "45%")
	if got.State != tasks.StateFailure {
		t.Fatalf("State = %q，期望 failure", got.State)
	}
	if got.RawState != "  CANCEL  " {
		t.Fatalf("RawState = %q，必须逐字保留上游原文", got.RawState)
	}
	if got.ProgressPercent != 45 {
		t.Fatalf("ProgressPercent = %d，期望 45", got.ProgressPercent)
	}

	unknown := NormalizeStatus("SOMETHING_NEW", "not-a-number")
	if unknown.State != tasks.StateUnknown || IsFailure(unknown.State) {
		t.Fatalf("unknown 状态不得计入失败：%+v", unknown)
	}
	if unknown.RawState != "SOMETHING_NEW" {
		t.Fatalf("未知状态的 raw 也必须保留，got %q", unknown.RawState)
	}
	if unknown.ProgressPercent != 0 {
		t.Fatalf("不可解析进度应归零，got %d", unknown.ProgressPercent)
	}

	// failure 才计入失败；MODAL 是非终态。
	if !IsFailure(tasks.StateFailure) {
		t.Fatalf("failure 应计入失败")
	}
	for _, state := range []tasks.State{
		tasks.StateNotStart, tasks.StateSubmitted, tasks.StateQueued,
		tasks.StateInProgress, tasks.StateSuccess, tasks.StateUnknown,
	} {
		if IsFailure(state) {
			t.Fatalf("%q 不得计入失败", state)
		}
	}
}

// TestParseProgress 覆盖 "N%" 解析与边界。
func TestParseProgress(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"0%", 0},
		{"45%", 45},
		{"100%", 100},
		{"45", 45},
		{"  45 %  ", 45},
		{"100.0%", 100},
		{"45.9%", 45},
		{"0.0%", 0},
		{"101%", 100}, // 钳制到 DB CHECK 上界
		{"-1%", 0},
		{"", 0},
		{"abc%", 0},
		{"NaN", 0},
	}
	for _, c := range cases {
		if got := ParseProgress(c.raw); got != c.want {
			t.Fatalf("ParseProgress(%q) = %d，期望 %d", c.raw, got, c.want)
		}
	}
}

// TestProjectStatus 钉死线协议投影：raw 优先，网关自设值逐字不变。
func TestProjectStatus(t *testing.T) {
	cases := []struct {
		state tasks.State
		raw   string
		want  string
	}{
		{tasks.StateNotStart, "", ""},
		{tasks.StateSuccess, "", "SUCCESS"},
		{tasks.StateFailure, "", "FAILURE"},
		{tasks.StateInProgress, "", ""},
		{tasks.StateUnknown, "", ""},
		// 上游回填状态逐字回传：CANCEL 不被改写成 FAILURE，空白也不修剪。
		{tasks.StateFailure, "CANCEL", "CANCEL"},
		{tasks.StateFailure, "  CANCEL  ", "  CANCEL  "},
		{tasks.StateInProgress, "MODAL", "MODAL"},
		{tasks.StateSuccess, "SUCCESS", "SUCCESS"},
	}
	for _, c := range cases {
		if got := ProjectStatus(c.state, c.raw); got != c.want {
			t.Fatalf("ProjectStatus(%q,%q) = %q，期望 %q", c.state, c.raw, got, c.want)
		}
	}
}

// TestProjectProgress 钉死 "N%" 线值与钳制。
func TestProjectProgress(t *testing.T) {
	cases := map[int]string{
		0: "0%", 45: "45%", 100: "100%", -1: "0%", 101: "100%",
	}
	for percent, want := range cases {
		if got := ProjectProgress(percent); got != want {
			t.Fatalf("ProjectProgress(%d) = %q，期望 %q", percent, got, want)
		}
	}
}
