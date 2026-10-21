package mj

import "testing"

// TestFrozenWireLiterals 用**字面量**冻结对外线协议，不引用常量自身。
//
// 常量的定义与常量的取值必须由两份独立断言同时钉住：只断言「常量 A == 常量 B」
// 或只通过使用常量的函数断言行为，改常量值时测试会随常量一起漂移（自证）。
// 本用例覆盖评审 I1 的四类变异：线码、错误描述、端点路径、动作名/模型名。
func TestFrozenWireLiterals(t *testing.T) {
	// 12 条提交端点：Endpoint 值（/mj/ 之后）与 Path() 都要逐字冻结。
	endpoints := []struct {
		endpoint Endpoint
		relative string
		path     string
	}{
		{EndpointImagine, "submit/imagine", "/mj/submit/imagine"},
		{EndpointChange, "submit/change", "/mj/submit/change"},
		{EndpointSimpleChange, "submit/simple-change", "/mj/submit/simple-change"},
		{EndpointAction, "submit/action", "/mj/submit/action"},
		{EndpointModal, "submit/modal", "/mj/submit/modal"},
		{EndpointShorten, "submit/shorten", "/mj/submit/shorten"},
		{EndpointDescribe, "submit/describe", "/mj/submit/describe"},
		{EndpointBlend, "submit/blend", "/mj/submit/blend"},
		{EndpointEdits, "submit/edits", "/mj/submit/edits"},
		{EndpointVideo, "submit/video", "/mj/submit/video"},
		{EndpointUploadDiscord, "submit/upload-discord-images", "/mj/submit/upload-discord-images"},
		{EndpointInsightFace, "insight-face/swap", "/mj/insight-face/swap"},
	}
	if len(Endpoints()) != len(endpoints) {
		t.Fatalf("端点数量 = %d，冻结值 12", len(Endpoints()))
	}
	for _, c := range endpoints {
		if string(c.endpoint) != c.relative {
			t.Fatalf("端点字面量 = %q，冻结值 %q", string(c.endpoint), c.relative)
		}
		if c.endpoint.Path() != c.path {
			t.Fatalf("%q.Path() = %q，冻结值 %q", c.endpoint, c.endpoint.Path(), c.path)
		}
	}

	// 18 个动作名与模型名（SWAP_FACE 是唯一无 mj_ 前缀的特例）。
	actions := []struct {
		action Action
		name   string
		model  string
	}{
		{ActionImagine, "IMAGINE", "mj_imagine"},
		{ActionDescribe, "DESCRIBE", "mj_describe"},
		{ActionBlend, "BLEND", "mj_blend"},
		{ActionUpscale, "UPSCALE", "mj_upscale"},
		{ActionVariation, "VARIATION", "mj_variation"},
		{ActionReroll, "REROLL", "mj_reroll"},
		{ActionInpaint, "INPAINT", "mj_inpaint"},
		{ActionModal, "MODAL", "mj_modal"},
		{ActionZoom, "ZOOM", "mj_zoom"},
		{ActionCustomZoom, "CUSTOM_ZOOM", "mj_custom_zoom"},
		{ActionShorten, "SHORTEN", "mj_shorten"},
		{ActionHighVariation, "HIGH_VARIATION", "mj_high_variation"},
		{ActionLowVariation, "LOW_VARIATION", "mj_low_variation"},
		{ActionPan, "PAN", "mj_pan"},
		{ActionSwapFace, "SWAP_FACE", "swap_face"},
		{ActionUpload, "UPLOAD", "mj_upload"},
		{ActionVideo, "VIDEO", "mj_video"},
		{ActionEdits, "EDITS", "mj_edits"},
	}
	if len(Actions()) != len(actions) {
		t.Fatalf("动作数量 = %d，冻结值 18", len(Actions()))
	}
	for _, c := range actions {
		if string(c.action) != c.name {
			t.Fatalf("动作字面量 = %q，冻结值 %q", string(c.action), c.name)
		}
		model, ok := c.action.Model()
		if !ok || model != c.model {
			t.Fatalf("%q.Model() = (%q,%v)，冻结值 (%q,true)", c.name, model, ok, c.model)
		}
	}

	// 9 个线协议返回码（R-MJ-17 / relay-05 §4.2）。
	codes := []struct {
		name string
		got  int
		want int
	}{
		{"CodeSuccess", CodeSuccess, 1},
		{"CodeNoAccount", CodeNoAccount, 3},
		{"CodeRequestError", CodeRequestError, 4},
		{"CodeUnknownError", CodeUnknownError, 5},
		{"CodeTaskExists", CodeTaskExists, 21},
		{"CodeQueued", CodeQueued, 22},
		{"CodeQueueFull", CodeQueueFull, 23},
		{"CodeSensitiveWord", CodeSensitiveWord, 24},
		{"CodeGroupSaturated", CodeGroupSaturated, 30},
	}
	for _, c := range codes {
		if c.got != c.want {
			t.Fatalf("%s = %d，冻结值 %d", c.name, c.got, c.want)
		}
	}

	// 固定错误描述：客户端/测试按字面量匹配，不得漂移。
	descriptions := []struct {
		name string
		got  string
		want string
	}{
		{"InvalidRequest", DescriptionInvalidRequest, "invalid_request"},
		{"UnknownEndpoint", DescriptionUnknownEndpoint, "unknown_endpoint"},
		{"UnknownAction", DescriptionUnknownAction, "unknown_action"},
		{"IndexParseFailed", DescriptionIndexParseFailed, "index_parse_failed"},
		{"PromptRequired", DescriptionPromptRequired, "prompt_is_required"},
		{"ContentRequired", DescriptionContentRequired, "content_is_required"},
		{"ContentParseFailed", DescriptionContentParseFailed, "content_parse_failed"},
		{"CustomIDRequired", DescriptionCustomIDRequired, "custom_id_is_required"},
		{"TaskIDRequired", DescriptionTaskIDRequired, "task_id_is_required"},
		{"ActionRequired", DescriptionActionRequired, "action_is_required"},
		{"IndexRequired", DescriptionIndexRequired, "index_is_required"},
		{"Base64Required", DescriptionBase64Required, "base64_array_is_required"},
		{"SourceRequired", DescriptionSourceRequired, "source_base64_is_required"},
		{"TargetRequired", DescriptionTargetRequired, "target_base64_is_required"},
		{"Base64ArrayTooLarge", DescriptionBase64ArrayTooLarge, "base64_array_too_large"},
		{"Base64ItemTooLarge", DescriptionBase64ItemTooLarge, "base64_item_too_large"},
		{"PromptTooLong", DescriptionPromptTooLong, "prompt_too_long"},
		{"ContentTooLong", DescriptionContentTooLong, "content_too_long"},
		{"CustomIDTooLong", DescriptionCustomIDTooLong, "custom_id_too_long"},
		{"MaskBase64TooLarge", DescriptionMaskBase64TooLarge, "mask_base64_too_large"},
		{"SourceTooLarge", DescriptionSourceTooLarge, "source_base64_too_large"},
		{"TargetTooLarge", DescriptionTargetTooLarge, "target_base64_too_large"},
		{"InvalidTaskID", DescriptionInvalidTaskID, "invalid_task_id"},
		{"TooManyIDs", DescriptionTooManyIDs, "too_many_ids"},
		{"InvalidID", DescriptionInvalidID, "invalid_id"},
		{"IdempotencyTooLong", DescriptionIdempotencyTooLong, "idempotency_key_too_long"},
		{"InvalidIdempotency", DescriptionInvalidIdempotency, "invalid_idempotency_key"},
		{"NoChannel", DescriptionNoChannel, "no_available_channel"},
		{"OriginTaskMissing", DescriptionOriginTaskMissing, "get_channel_info_failed"},
		{"ChannelDisabled", DescriptionChannelDisabled, "channel_disabled"},
		{"TaskNotSuccess", DescriptionTaskNotSuccess, "action_task_not_success"},
		{"InternalError", DescriptionInternalError, "internal_error"},
		// W8/MJ-a2：提交编排新增的固定描述（16 个）。handler 测试用字面量
		// 断言，常量本身的取值由本表独立钉住（M2）。
		{"Disabled", DescriptionDisabled, "mj_not_enabled"},
		{"PlusChannelRequired", DescriptionPlusChannelRequired, "plus_channel_required"},
		{"QuotaNotEnough", DescriptionQuotaNotEnough, "quota_not_enough"},
		{"PricingUnavailable", DescriptionPricingUnavailable, "task_pricing_unavailable"},
		{"BodyTooLarge", DescriptionBodyTooLarge, "request_body_too_large"},
		{"ChannelMisconfigured", DescriptionChannelMisconfigured, "channel_misconfigured"},
		{"UpstreamError", DescriptionUpstreamError, "upstream_error"},
		{"UpstreamBadResponse", DescriptionUpstreamBadResponse, "upstream_bad_response"},
		{"UpstreamResponseTooLarge", DescriptionUpstreamResponseTooLarge, "upstream_response_too_large"},
		{"NoAccount", DescriptionNoAccount, "no_account_instance"},
		{"SensitiveWord", DescriptionSensitiveWord, "sensitive_word_detected"},
		{"GroupSaturated", DescriptionGroupSaturated, "group_saturated"},
		{"SubmitFailed", DescriptionSubmitFailed, "submit_failed"},
		{"InvalidToken", DescriptionInvalidToken, "invalid_token"},
		{"Forbidden", DescriptionForbidden, "forbidden"},
		{"RateLimited", DescriptionRateLimited, "rate_limited"},
	}
	for _, c := range descriptions {
		if c.got != c.want {
			t.Fatalf("%s = %q，冻结值 %q", c.name, c.got, c.want)
		}
	}
}
