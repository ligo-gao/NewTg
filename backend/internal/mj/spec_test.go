package mj

import (
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// TestFrozenBoundConstants 钉死 R-MJ-16 的常量取值本身。
//
// 边界用例使用字面量（32*1024、8<<20…）而不是引用常量：否则把常量放宽
// 后，`上限+1` 会跟着漂移，测试永远绿——这正是变异测试要抓的自证循环。
// 本用例负责在「常量被改」时变红，边界用例负责在「校验被放宽」时变红。
func TestFrozenBoundConstants(t *testing.T) {
	if MaxBase64Items != 8 {
		t.Fatalf("MaxBase64Items = %d，冻结值 8", MaxBase64Items)
	}
	if MaxBase64ItemBytes != 8<<20 {
		t.Fatalf("MaxBase64ItemBytes = %d，冻结值 8MiB", MaxBase64ItemBytes)
	}
	if MaxPromptBytes != 32*1024 {
		t.Fatalf("MaxPromptBytes = %d，冻结值 32KiB", MaxPromptBytes)
	}
	if MaxContentBytes != 1024 {
		t.Fatalf("MaxContentBytes = %d，冻结值 1KiB", MaxContentBytes)
	}
	if MaxCustomIDBytes != 1024 {
		t.Fatalf("MaxCustomIDBytes = %d，冻结值 1KiB", MaxCustomIDBytes)
	}
	if MaxListIDs != 100 {
		t.Fatalf("MaxListIDs = %d，冻结值 100", MaxListIDs)
	}
	if MaxMjIDLen != 128 {
		t.Fatalf("MaxMjIDLen = %d，冻结值 128", MaxMjIDLen)
	}
	if MaxIdempotencyKey != 1024 {
		t.Fatalf("MaxIdempotencyKey = %d，冻结值 1KiB", MaxIdempotencyKey)
	}
	if MaxOwnerCandidates != 100 {
		t.Fatalf("MaxOwnerCandidates = %d，冻结值 100", MaxOwnerCandidates)
	}
}

// TestNormalizeMinimalRequests 逐端点验证最小合法请求的动作/模型/锁定语义。
func TestNormalizeMinimalRequests(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   Endpoint
		req        *SubmitRequest
		wantAction Action
		wantModel  string
		wantTask   string
		wantPlus   bool
		wantLock   bool
	}{
		{
			name: "imagine", endpoint: EndpointImagine,
			req:        &SubmitRequest{Prompt: ptr("a cat")},
			wantAction: ActionImagine, wantModel: "mj_imagine",
		},
		{
			name: "change", endpoint: EndpointChange,
			req:        &SubmitRequest{TaskID: ptr("abc"), Action: ptr("UPSCALE"), Index: ptr(1)},
			wantAction: ActionUpscale, wantModel: "mj_upscale", wantTask: "abc", wantLock: true,
		},
		{
			name: "simple-change", endpoint: EndpointSimpleChange,
			req:        &SubmitRequest{Content: ptr("abc v2")},
			wantAction: ActionVariation, wantModel: "mj_variation", wantTask: "abc", wantLock: true,
		},
		{
			name: "action without task", endpoint: EndpointAction,
			req:        &SubmitRequest{CustomID: ptr("MJ::JOB::pan::::uuid")},
			wantAction: ActionPan, wantModel: "mj_pan", wantPlus: true,
		},
		{
			name: "action with task", endpoint: EndpointAction,
			req:        &SubmitRequest{CustomID: ptr("MJ::JOB::pan::::uuid"), TaskID: ptr("abc")},
			wantAction: ActionPan, wantModel: "mj_pan", wantTask: "abc", wantPlus: true, wantLock: true,
		},
		{
			name: "modal", endpoint: EndpointModal,
			req:        &SubmitRequest{TaskID: ptr("abc")},
			wantAction: ActionModal, wantModel: "mj_modal", wantTask: "abc", wantPlus: true, wantLock: true,
		},
		{
			name: "shorten", endpoint: EndpointShorten,
			req:        &SubmitRequest{Prompt: ptr("a prompt")},
			wantAction: ActionShorten, wantModel: "mj_shorten", wantPlus: true,
		},
		{
			name: "describe", endpoint: EndpointDescribe,
			req:        &SubmitRequest{Base64Array: []string{"aGVsbG8="}},
			wantAction: ActionDescribe, wantModel: "mj_describe",
		},
		{
			name: "blend", endpoint: EndpointBlend,
			req:        &SubmitRequest{Base64Array: []string{"aGVsbG8=", "d29ybGQ="}},
			wantAction: ActionBlend, wantModel: "mj_blend",
		},
		{
			name: "edits", endpoint: EndpointEdits,
			req:        &SubmitRequest{Prompt: ptr("edit it")},
			wantAction: ActionEdits, wantModel: "mj_edits",
		},
		{
			name: "video forces VIDEO", endpoint: EndpointVideo,
			req:        &SubmitRequest{TaskID: ptr("abc"), Action: ptr("UPSCALE")},
			wantAction: ActionVideo, wantModel: "mj_video", wantTask: "abc", wantLock: true,
		},
		{
			name: "upload", endpoint: EndpointUploadDiscord,
			req:        &SubmitRequest{Base64Array: []string{"aGVsbG8="}},
			wantAction: ActionUpload, wantModel: "mj_upload",
		},
		{
			name: "swap face", endpoint: EndpointInsightFace,
			req:        &SubmitRequest{SourceBase64: ptr("source"), TargetBase64: ptr("target")},
			wantAction: ActionSwapFace, wantModel: "swap_face", wantPlus: true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := Normalize(c.endpoint, c.req, Options{})
			if err != nil {
				t.Fatalf("Normalize error: %v", err)
			}
			if got.Action != c.wantAction || got.Model != c.wantModel {
				t.Fatalf("action/model = (%s,%s)，期望 (%s,%s)",
					got.Action, got.Model, c.wantAction, c.wantModel)
			}
			if got.TaskID != c.wantTask {
				t.Fatalf("TaskID = %q，期望 %q", got.TaskID, c.wantTask)
			}
			if got.PlusOnly != c.wantPlus {
				t.Fatalf("PlusOnly = %v，期望 %v", got.PlusOnly, c.wantPlus)
			}
			if got.LockOrigin != c.wantLock {
				t.Fatalf("LockOrigin = %v，期望 %v", got.LockOrigin, c.wantLock)
			}
		})
	}
}

// TestNormalizeRejectsUnknownEndpoint 未知端点/空请求必须 400。
func TestNormalizeRejectsUnknownEndpoint(t *testing.T) {
	if _, err := Normalize(Endpoint("submit/notify"), &SubmitRequest{}, Options{}); err == nil ||
		err.Description != DescriptionUnknownEndpoint {
		t.Fatalf("未知端点应返回 unknown_endpoint，got %#v", err)
	}
	if _, err := Normalize(EndpointImagine, nil, Options{}); err == nil ||
		err.Description != DescriptionInvalidRequest {
		t.Fatalf("nil 请求应返回 invalid_request，got %#v", err)
	}
}

// TestNormalizeRequiredFields 逐端点必填错误。
func TestNormalizeRequiredFields(t *testing.T) {
	cases := []struct {
		name     string
		endpoint Endpoint
		req      *SubmitRequest
		want     string
	}{
		{"imagine prompt", EndpointImagine, &SubmitRequest{}, DescriptionPromptRequired},
		{"imagine blank prompt", EndpointImagine, &SubmitRequest{Prompt: ptr("   ")}, DescriptionPromptRequired},
		{"shorten prompt", EndpointShorten, &SubmitRequest{}, DescriptionPromptRequired},
		{"edits prompt", EndpointEdits, &SubmitRequest{}, DescriptionPromptRequired},
		{"change task id", EndpointChange, &SubmitRequest{Action: ptr("UPSCALE"), Index: ptr(1)}, DescriptionTaskIDRequired},
		{"change action", EndpointChange, &SubmitRequest{TaskID: ptr("abc"), Index: ptr(1)}, DescriptionActionRequired},
		{"change index nil", EndpointChange, &SubmitRequest{TaskID: ptr("abc"), Action: ptr("UPSCALE")}, DescriptionIndexRequired},
		{"change index zero", EndpointChange, &SubmitRequest{TaskID: ptr("abc"), Action: ptr("UPSCALE"), Index: ptr(0)}, DescriptionIndexRequired},
		{"simple-change content", EndpointSimpleChange, &SubmitRequest{}, DescriptionContentRequired},
		{"action custom id", EndpointAction, &SubmitRequest{}, DescriptionCustomIDRequired},
		{"modal task id", EndpointModal, &SubmitRequest{}, DescriptionTaskIDRequired},
		{"video task id", EndpointVideo, &SubmitRequest{}, DescriptionTaskIDRequired},
		{"describe base64", EndpointDescribe, &SubmitRequest{}, DescriptionBase64Required},
		{"blend base64", EndpointBlend, &SubmitRequest{}, DescriptionBase64Required},
		{"upload base64", EndpointUploadDiscord, &SubmitRequest{}, DescriptionBase64Required},
		{"swap source", EndpointInsightFace, &SubmitRequest{TargetBase64: ptr("t")}, DescriptionSourceRequired},
		{"swap target", EndpointInsightFace, &SubmitRequest{SourceBase64: ptr("s")}, DescriptionTargetRequired},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := Normalize(c.endpoint, c.req, Options{})
			if err == nil {
				t.Fatalf("期望 %s，实际成功: %+v", c.want, got)
			}
			if err.Code != CodeRequestError || err.HTTPStatus() != 400 || err.Description != c.want {
				t.Fatalf("err = %#v，期望 code=4 status=400 description=%s", err, c.want)
			}
		})
	}
}

// TestNormalizeInputBounds 是 R-MJ-16 的边界矩阵：恰好在上界必须通过，
// 上界 +1 必须返回固定 400。放宽任一上界都会让对应行变红。
func TestNormalizeInputBounds(t *testing.T) {
	bigBase64 := func(n int) string { return strings.Repeat("A", n) }

	// ---- 恰好在上界：全部必须成功 ----
	okCases := []struct {
		name     string
		endpoint Endpoint
		req      *SubmitRequest
	}{
		{"prompt 32KiB", EndpointImagine, &SubmitRequest{Prompt: ptr(strings.Repeat("p", 32*1024))}},
		{"content 1KiB", EndpointImagine, &SubmitRequest{Prompt: ptr("ok"), Content: ptr(strings.Repeat("c", 1024))}},
		{"customId 1KiB", EndpointImagine, &SubmitRequest{Prompt: ptr("ok"), CustomID: ptr(strings.Repeat("c", 1024))}},
		{"base64 8 items", EndpointDescribe, &SubmitRequest{Base64Array: make([]string, 8)}},
		{"base64 item 8MiB", EndpointDescribe, &SubmitRequest{Base64Array: []string{bigBase64(8 << 20)}}},
		{"mask 8MiB", EndpointImagine, &SubmitRequest{Prompt: ptr("ok"), MaskBase64: ptr(bigBase64(8 << 20))}},
		{"source 8MiB", EndpointInsightFace, &SubmitRequest{SourceBase64: ptr(bigBase64(8 << 20)), TargetBase64: ptr("t")}},
		{"target 8MiB", EndpointInsightFace, &SubmitRequest{SourceBase64: ptr("s"), TargetBase64: ptr(bigBase64(8 << 20))}},
		{"task id 128 chars", EndpointChange, &SubmitRequest{TaskID: ptr(strings.Repeat("a", 128)), Action: ptr("UPSCALE"), Index: ptr(1)}},
	}
	for _, c := range okCases {
		c := c
		t.Run("ok/"+c.name, func(t *testing.T) {
			if _, err := Normalize(c.endpoint, c.req, Options{}); err != nil {
				t.Fatalf("边界内请求被拒: %v", err)
			}
		})
	}

	// ---- 上界 +1：全部必须 400，且描述是固定的 ----
	tooLarge := []struct {
		name     string
		endpoint Endpoint
		req      *SubmitRequest
		want     string
	}{
		{
			"prompt 32KiB+1", EndpointImagine,
			&SubmitRequest{Prompt: ptr(strings.Repeat("p", 32*1024+1))},
			DescriptionPromptTooLong,
		},
		{
			"content 1KiB+1", EndpointImagine,
			&SubmitRequest{Prompt: ptr("ok"), Content: ptr(strings.Repeat("c", 1024+1))},
			DescriptionContentTooLong,
		},
		{
			"customId 1KiB+1", EndpointImagine,
			&SubmitRequest{Prompt: ptr("ok"), CustomID: ptr(strings.Repeat("c", 1024+1))},
			DescriptionCustomIDTooLong,
		},
		{
			"base64 9 items", EndpointDescribe,
			&SubmitRequest{Base64Array: make([]string, 8+1)},
			DescriptionBase64ArrayTooLarge,
		},
		{
			"base64 item 8MiB+1", EndpointDescribe,
			&SubmitRequest{Base64Array: []string{bigBase64(8<<20 + 1)}},
			DescriptionBase64ItemTooLarge,
		},
		{
			"mask 8MiB+1", EndpointImagine,
			&SubmitRequest{Prompt: ptr("ok"), MaskBase64: ptr(bigBase64(8<<20 + 1))},
			DescriptionMaskBase64TooLarge,
		},
		{
			"source 8MiB+1", EndpointInsightFace,
			&SubmitRequest{SourceBase64: ptr(bigBase64(8<<20 + 1)), TargetBase64: ptr("t")},
			DescriptionSourceTooLarge,
		},
		{
			"target 8MiB+1", EndpointInsightFace,
			&SubmitRequest{SourceBase64: ptr("s"), TargetBase64: ptr(bigBase64(8<<20 + 1))},
			DescriptionTargetTooLarge,
		},
		{
			"task id 129 chars", EndpointChange,
			&SubmitRequest{TaskID: ptr(strings.Repeat("a", 128+1)), Action: ptr("UPSCALE"), Index: ptr(1)},
			DescriptionInvalidTaskID,
		},
		{
			"task id illegal chars", EndpointChange,
			&SubmitRequest{TaskID: ptr("abc/../def"), Action: ptr("UPSCALE"), Index: ptr(1)},
			DescriptionInvalidTaskID,
		},
	}
	for _, c := range tooLarge {
		c := c
		t.Run("too-large/"+c.name, func(t *testing.T) {
			_, err := Normalize(c.endpoint, c.req, Options{})
			if err == nil {
				t.Fatalf("超限请求未被拒绝")
			}
			if err.Code != CodeRequestError || err.HTTPStatus() != 400 || err.Description != c.want {
				t.Fatalf("err = %#v，期望 400/%s", err, c.want)
			}
			// 固定错误不得回显用户输入（这里是超长输入本身）。
			if strings.Contains(err.Description, "ppp") || strings.Contains(err.Description, "AAA") {
				t.Fatalf("错误描述泄露了用户输入: %q", err.Description)
			}
		})
	}
}

// TestNormalizeUnknownAction 未知动作必须 400 unknown_action。
func TestNormalizeUnknownAction(t *testing.T) {
	_, err := Normalize(EndpointChange, &SubmitRequest{
		TaskID: ptr("abc"), Action: ptr("NOT_A_REAL_ACTION"), Index: ptr(1),
	}, Options{})
	if err == nil || err.Code != CodeRequestError || err.Description != DescriptionUnknownAction {
		t.Fatalf("未知动作应返回 unknown_action，got %#v", err)
	}
}

// TestNormalizeOptions 钉死 accountFilter/notifyHook 默认剥离与模式标志清理。
func TestNormalizeOptions(t *testing.T) {
	req := func() *SubmitRequest {
		return &SubmitRequest{
			Prompt:        ptr("a cat --fast --relax --turbo"),
			AccountFilter: map[string]any{"instanceId": "x"},
			NotifyHook:    ptr("https://client.example/hook"),
		}
	}

	// 默认零值：全部关闭（剥离 + 保留标志）。
	original := req()
	got, err := Normalize(EndpointImagine, original, Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.AccountFilter != nil {
		t.Fatalf("accountFilter 默认必须剥离，got %#v", got.AccountFilter)
	}
	if got.NotifyHook != nil {
		t.Fatalf("notifyHook 默认必须剥离，got %#v", got.NotifyHook)
	}
	if got.Prompt == nil || !strings.Contains(*got.Prompt, "--fast") {
		t.Fatalf("默认不得清理模式标志，got %v", got.Prompt)
	}

	// 全部开启：透传 + 清理。
	original = req()
	got, err = Normalize(EndpointImagine, original, Options{
		ForwardAccountFilter: true,
		ForwardNotifyHook:    true,
		ClearModeFlags:       true,
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.AccountFilter == nil || got.AccountFilter["instanceId"] != "x" {
		t.Fatalf("accountFilter 应透传，got %#v", got.AccountFilter)
	}
	if got.NotifyHook == nil || *got.NotifyHook != "https://client.example/hook" {
		t.Fatalf("notifyHook 应透传，got %#v", got.NotifyHook)
	}
	if got.Prompt == nil {
		t.Fatalf("prompt 丢失")
	}
	for _, flag := range []string{"--fast", "--relax", "--turbo"} {
		if strings.Contains(*got.Prompt, flag) {
			t.Fatalf("模式标志 %s 未被清理: %q", flag, *got.Prompt)
		}
	}
	if !strings.Contains(*got.Prompt, "a cat") {
		t.Fatalf("提示词正文被破坏: %q", *got.Prompt)
	}
	// 只有完整 token 才是模式标志：--fastest 必须原样保留。
	similar := &SubmitRequest{Prompt: ptr("make it --fastest")}
	keptPrompt, err := Normalize(EndpointImagine, similar, Options{ClearModeFlags: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if keptPrompt.Prompt == nil || !strings.Contains(*keptPrompt.Prompt, "--fastest") {
		t.Fatalf("--fastest 被误删: %v", keptPrompt.Prompt)
	}
	// 归一化不得原地修改调用方请求（日志/审计可能复用）。
	if !strings.Contains(*original.Prompt, "--fast") {
		t.Fatalf("归一化原地修改了调用方 prompt: %q", *original.Prompt)
	}
}

// TestNormalizePointerSemantics 指针字段：缺席与显式空值必须可区分。
func TestNormalizePointerSemantics(t *testing.T) {
	got, err := Normalize(EndpointImagine, &SubmitRequest{
		Prompt:  ptr("x"),
		BotType: ptr(""),
		State:   ptr(""),
	}, Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.BotType == nil || *got.BotType != "" {
		t.Fatalf("显式空 botType 必须保留为空串指针，got %#v", got.BotType)
	}
	if got.State == nil || *got.State != "" {
		t.Fatalf("显式空 state 必须保留为空串指针，got %#v", got.State)
	}

	absent, err := Normalize(EndpointImagine, &SubmitRequest{Prompt: ptr("x")}, Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if absent.BotType != nil || absent.State != nil {
		t.Fatalf("缺席字段必须保持 nil，got botType=%#v state=%#v", absent.BotType, absent.State)
	}
}

// TestParseIdempotencyKey 钉死可选幂等键的解析与上界（R-MJ-06 第一步）。
func TestParseIdempotencyKey(t *testing.T) {
	if IdempotencyKeyHeader != "Idempotency-Key" {
		t.Fatalf("冻结头名被改动: %q", IdempotencyKeyHeader)
	}

	key, present, err := ParseIdempotencyKey("")
	if err != nil || present || key != "" {
		t.Fatalf("空键 = (%q,%v,%v)，期望未提供", key, present, err)
	}
	key, present, err = ParseIdempotencyKey("   ")
	if err != nil || present || key != "" {
		t.Fatalf("空白键 = (%q,%v,%v)，期望未提供", key, present, err)
	}
	key, present, err = ParseIdempotencyKey("  abc-123  ")
	if err != nil || !present || key != "abc-123" {
		t.Fatalf("普通键 = (%q,%v,%v)，期望 abc-123", key, present, err)
	}

	if _, _, err := ParseIdempotencyKey(strings.Repeat("k", 1<<10)); err != nil {
		t.Fatalf("恰好上界应通过: %v", err)
	}
	if _, _, err := ParseIdempotencyKey(strings.Repeat("k", 1<<10+1)); err == nil ||
		err.Description != DescriptionIdempotencyTooLong {
		t.Fatalf("超长键应返回 idempotency_key_too_long，got %#v", err)
	}
	if _, _, err := ParseIdempotencyKey("abc\n123"); err == nil ||
		err.Description != DescriptionInvalidIdempotency {
		t.Fatalf("控制字符应返回 invalid_idempotency_key，got %#v", err)
	}
}
