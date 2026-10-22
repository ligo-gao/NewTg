package mj

import (
	"strings"
	"testing"
)

// TestActionModelTable 逐条钉死 relay-05 §3.2 的动作 → 模型名映射。
//
// 这是渠道选择、/v1/models 下发与计价的唯一权威表；删一项、改一项或把
// SWAP_FACE 拼成 mj_swap_face，都必须让本用例变红。
func TestActionModelTable(t *testing.T) {
	want := map[Action]string{
		ActionImagine:       "mj_imagine",
		ActionDescribe:      "mj_describe",
		ActionBlend:         "mj_blend",
		ActionUpscale:       "mj_upscale",
		ActionVariation:     "mj_variation",
		ActionReroll:        "mj_reroll",
		ActionInpaint:       "mj_inpaint",
		ActionModal:         "mj_modal",
		ActionZoom:          "mj_zoom",
		ActionCustomZoom:    "mj_custom_zoom",
		ActionShorten:       "mj_shorten",
		ActionHighVariation: "mj_high_variation",
		ActionLowVariation:  "mj_low_variation",
		ActionPan:           "mj_pan",
		ActionSwapFace:      "swap_face",
		ActionUpload:        "mj_upload",
		ActionVideo:         "mj_video",
		ActionEdits:         "mj_edits",
	}
	if len(actionModels) != len(want) || len(want) != 18 {
		t.Fatalf("动作表大小 = %d（常量 %d），期望 18", len(actionModels), len(want))
	}
	for action, model := range want {
		got, ok := action.Model()
		if !ok || got != model {
			t.Fatalf("%s.Model() = (%q,%v)，期望 (%q,true)", action, got, ok, model)
		}
		if !action.Valid() {
			t.Fatalf("%s.Valid() = false", action)
		}
	}
	if _, ok := Action("NOPE").Model(); ok {
		t.Fatalf("未知动作不应有模型名")
	}
	if Action("NOPE").Valid() {
		t.Fatalf("未知动作不合法")
	}
}

// TestEndpointsComplete 钉死 12 条提交端点与规范路径。
func TestEndpointsComplete(t *testing.T) {
	want := []Endpoint{
		EndpointImagine, EndpointChange, EndpointSimpleChange, EndpointAction,
		EndpointModal, EndpointShorten, EndpointDescribe, EndpointBlend,
		EndpointEdits, EndpointVideo, EndpointUploadDiscord, EndpointInsightFace,
	}
	got := Endpoints()
	if len(got) != 12 || len(want) != 12 {
		t.Fatalf("端点数量 = %d，期望 12", len(got))
	}
	seen := make(map[Endpoint]bool, len(want))
	for _, endpoint := range want {
		if !endpoint.Valid() {
			t.Fatalf("%q 不是合法端点", endpoint)
		}
		if seen[endpoint] {
			t.Fatalf("端点 %q 重复", endpoint)
		}
		seen[endpoint] = true
		path := endpoint.Path()
		if !strings.HasPrefix(path, "/mj/") {
			t.Fatalf("%q.Path() = %q，必须以 /mj/ 开头", endpoint, path)
		}
	}
	if Endpoint("submit/notify").Valid() {
		t.Fatalf("休眠端点 /mj/notify 不得进提交端点集合")
	}

	// 返回的是副本：调用方原地修改不得污染包内清单（a2 依赖它注册路由）。
	got[0] = "mutated"
	if Endpoints()[0] == "mutated" {
		t.Fatalf("Endpoints() 返回了共享切片")
	}
	actions := Actions()
	if len(actions) != 18 {
		t.Fatalf("Actions() = %d 项，期望 18", len(actions))
	}
	actions[0] = "mutated"
	if Actions()[0] == "mutated" {
		t.Fatalf("Actions() 返回了共享切片")
	}
}

// TestResolveFixedActions 钉死固定动作端点；派生端点必须返回派生结果。
func TestResolveFixedActions(t *testing.T) {
	cases := []struct {
		endpoint Endpoint
		want     Action
	}{
		{EndpointImagine, ActionImagine},
		{EndpointModal, ActionModal},
		{EndpointShorten, ActionShorten},
		{EndpointDescribe, ActionDescribe},
		{EndpointBlend, ActionBlend},
		{EndpointEdits, ActionEdits},
		{EndpointUploadDiscord, ActionUpload},
		{EndpointInsightFace, ActionSwapFace},
	}
	for _, c := range cases {
		got, err := ResolveAction(c.endpoint, &SubmitRequest{})
		if err != nil {
			t.Fatalf("%s: %v", c.endpoint, err)
		}
		if got.Action != c.want || got.Index != 0 {
			t.Fatalf("%s: got (%s,%d)，期望 (%s,0)", c.endpoint, got.Action, got.Index, c.want)
		}
	}

	// video 强制 VIDEO，忽略客户端的 action（relay-05 §2.2）。
	clientAction := "UPSCALE"
	got, err := ResolveAction(EndpointVideo, &SubmitRequest{Action: &clientAction})
	if err != nil || got.Action != ActionVideo {
		t.Fatalf("video 必须强制 VIDEO，got=%v err=%v", got.Action, err)
	}

	// change 从 action 字段派生；未知动作 → unknown_action。
	action := "variation"
	index := 3
	got, err = ResolveAction(EndpointChange, &SubmitRequest{Action: &action, Index: &index})
	if err != nil || got.Action != ActionVariation || got.Index != 3 {
		t.Fatalf("change 派生 = (%v,%d,%v)", got.Action, got.Index, err)
	}
	unknown := "not_an_action"
	if _, err := ResolveAction(EndpointChange, &SubmitRequest{Action: &unknown}); err == nil ||
		err.Code != CodeRequestError || err.Description != DescriptionUnknownAction {
		t.Fatalf("未知动作应返回 unknown_action，got %#v", err)
	}

	// simple-change 从 content 派生。
	content := "abcDEF_123 v4"
	got, err = ResolveAction(EndpointSimpleChange, &SubmitRequest{Content: &content})
	if err != nil || got.Action != ActionVariation || got.Index != 4 {
		t.Fatalf("simple-change 派生 = (%v,%d,%v)", got.Action, got.Index, err)
	}

	// action 端点从 customId 派生。
	custom := "MJ::JOB::upsample::2::uuid"
	got, err = ResolveAction(EndpointAction, &SubmitRequest{CustomID: &custom})
	if err != nil || got.Action != ActionUpscale || got.Index != 2 {
		t.Fatalf("action 派生 = (%v,%d,%v)", got.Action, got.Index, err)
	}

	if _, err := ResolveAction(Endpoint("bogus"), &SubmitRequest{}); err == nil ||
		err.Description != DescriptionUnknownEndpoint {
		t.Fatalf("未知端点应返回 unknown_endpoint，got %#v", err)
	}
}

// TestPlusOnlyEndpoints 钉死 Plus-only 是**端点**能力（不是动作能力）。
func TestPlusOnlyEndpoints(t *testing.T) {
	plus := map[Endpoint]bool{
		EndpointAction: true, EndpointModal: true,
		EndpointShorten: true, EndpointInsightFace: true,
	}
	for _, endpoint := range Endpoints() {
		if got := RequiresPlus(endpoint); got != plus[endpoint] {
			t.Fatalf("%s RequiresPlus = %v，期望 %v", endpoint, got, plus[endpoint])
		}
	}
}

// TestLocksOriginMatrix 钉死动作类端点的原渠道锁定判定。
func TestLocksOriginMatrix(t *testing.T) {
	cases := []struct {
		endpoint Endpoint
		taskID   string
		want     bool
	}{
		{EndpointChange, "abc", true},
		{EndpointSimpleChange, "abc", true},
		// simple-change 的 taskId 在 content 里，端点本身恒为动作类。
		{EndpointSimpleChange, "", true},
		{EndpointModal, "abc", true},
		{EndpointVideo, "abc", true},
		{EndpointAction, "abc", true},
		{EndpointAction, "", false},
		{EndpointImagine, "abc", false},
		{EndpointEdits, "abc", false},
		{EndpointDescribe, "", false},
	}
	for _, c := range cases {
		if got := LocksOrigin(c.endpoint, c.taskID); got != c.want {
			t.Fatalf("LocksOrigin(%s,%q) = %v，期望 %v", c.endpoint, c.taskID, got, c.want)
		}
	}
}

// TestParseCustomID 覆盖 Plus customId 的全部分支（relay-05 §3.2）。
func TestParseCustomID(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Action
		index   int
		wantErr string
	}{
		{name: "upsample", raw: "MJ::JOB::upsample::1::uuid-1", want: ActionUpscale, index: 1},
		{name: "upsample 4", raw: "MJ::JOB::upsample::4::uuid", want: ActionUpscale, index: 4},
		{name: "variation", raw: "MJ::JOB::variation::2::uuid", want: ActionVariation, index: 2},
		{name: "low variation", raw: "MJ::JOB::low_variation::3::uuid", want: ActionLowVariation, index: 1},
		{name: "high variation", raw: "MJ::JOB::high_variation::3::uuid", want: ActionHighVariation, index: 1},
		{name: "pan", raw: "MJ::JOB::pan::::uuid", want: ActionPan, index: 1},
		{name: "reroll", raw: "MJ::JOB::reroll::::uuid", want: ActionReroll, index: 1},
		{name: "outpaint", raw: "MJ::JOB::Outpaint::1::uuid", want: ActionZoom, index: 1},
		{name: "custom zoom", raw: "MJ::JOB::CustomZoom::1::uuid", want: ActionCustomZoom, index: 1},
		{name: "inpaint", raw: "MJ::JOB::Inpaint::1::uuid", want: ActionInpaint, index: 1},
		{name: "alt form", raw: "MJ::pan::1::uuid", want: ActionPan, index: 1},
		{name: "empty", raw: "", wantErr: DescriptionCustomIDRequired},
		{name: "unknown action", raw: "MJ::JOB::mystery::1::uuid", wantErr: DescriptionUnknownAction},
		{name: "micro variation not guessed", raw: "MJ::JOB::micro_variation::1::uuid", wantErr: DescriptionUnknownAction},
		{name: "index missing", raw: "MJ::JOB::upsample", wantErr: DescriptionIndexParseFailed},
		{name: "index not numeric", raw: "MJ::JOB::upsample::x::uuid", wantErr: DescriptionIndexParseFailed},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			action, index, err := ParseCustomID(c.raw)
			if c.wantErr != "" {
				if err == nil || err.Description != c.wantErr || err.Code != CodeRequestError {
					t.Fatalf("ParseCustomID(%q) err = %#v，期望 %s", c.raw, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCustomID(%q) unexpected err %v", c.raw, err)
			}
			if action != c.want || index != c.index {
				t.Fatalf("ParseCustomID(%q) = (%s,%d)，期望 (%s,%d)", c.raw, action, index, c.want, c.index)
			}
		})
	}
}

// TestParseSimpleChange 覆盖 u/v × 1–4、r 与全部非法形态。
func TestParseSimpleChange(t *testing.T) {
	valid := []struct {
		content string
		action  Action
		index   int
	}{
		{"abc u1", ActionUpscale, 1},
		{"abc u2", ActionUpscale, 2},
		{"abc u3", ActionUpscale, 3},
		{"abc u4", ActionUpscale, 4},
		{"abc v1", ActionVariation, 1},
		{"abc v2", ActionVariation, 2},
		{"abc v3", ActionVariation, 3},
		{"abc v4", ActionVariation, 4},
		{"abc r", ActionReroll, 0},
		{"  abc   U2  ", ActionUpscale, 2},
	}
	for _, c := range valid {
		taskID, action, index, err := ParseSimpleChange(c.content)
		if err != nil {
			t.Fatalf("%q unexpected err %v", c.content, err)
		}
		if taskID != "abc" || action != c.action || index != c.index {
			t.Fatalf("%q = (%q,%s,%d)，期望 (abc,%s,%d)", c.content, taskID, action, index, c.action, c.index)
		}
	}

	invalid := []string{
		"", "abc", "abc x1", "abc u0", "abc u5", "abc u10", "abc r1",
		"abc v", "abc uv", "abc /u1", "bad/id u1", "abc u1 extra",
	}
	for _, content := range invalid {
		if _, _, _, err := ParseSimpleChange(content); err == nil ||
			err.Description != DescriptionContentParseFailed || err.Code != CodeRequestError {
			t.Fatalf("%q 应返回 content_parse_failed，got %#v", content, err)
		}
	}
}

// TestActionPriceTiers 钉死 relay-05 §7.2 的四档映射：18 个动作全覆盖、
// 无遗漏无重叠，且免计费档只能是 INPAINT / CUSTOM_ZOOM。
//
// 变异点：删/改任一条档位（例如把 INPAINT 标成付费、把 VIDEO 降档）都会
// 让该用例红；把未知动作默认成免费档会被 TestUnknownActionIsNotFree 抓住。
func TestActionPriceTiers(t *testing.T) {
	want := map[PriceTier][]Action{
		TierFree:     {ActionInpaint, ActionCustomZoom},
		TierLow:      {ActionDescribe, ActionUpscale, ActionSwapFace, ActionUpload},
		TierStandard: {ActionImagine, ActionBlend, ActionVariation, ActionReroll, ActionModal, ActionZoom, ActionShorten, ActionHighVariation, ActionLowVariation, ActionPan, ActionEdits},
		TierVideo:    {ActionVideo},
	}
	seen := map[Action]bool{}
	for tier, actions := range want {
		for _, action := range actions {
			if seen[action] {
				t.Fatalf("动作 %s 被重复归档", action)
			}
			seen[action] = true
			got, ok := action.PriceTier()
			if !ok || got != tier {
				t.Fatalf("%s 档位 = (%v,%v)，期望 %v", action, got, ok, tier)
			}
			if (tier == TierFree) != got.Free() {
				t.Fatalf("%s Free() = %v，档位 = %v", action, got.Free(), got)
			}
		}
	}
	// 与权威动作表逐一对账：每个动作都必须有档位，且集合相等。
	for _, action := range Actions() {
		if !seen[action] {
			t.Fatalf("动作 %s 没有计费档位", action)
		}
	}
	if len(seen) != len(Actions()) {
		t.Fatalf("档位表覆盖 %d 个动作，权威表 %d 个", len(seen), len(Actions()))
	}
}

// TestUnknownActionIsNotFree 断言未知动作不会被猜成免计费档。
func TestUnknownActionIsNotFree(t *testing.T) {
	if tier, ok := Action("NOT_AN_ACTION").PriceTier(); ok {
		t.Fatalf("未知动作返回档位 %v，必须返回 false", tier)
	}
}
