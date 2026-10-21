package mj

import (
	"testing"
	"time"

	"newtg/gateway/internal/tasks"
)

// TestTaskRowMapping 钉死 R-MJ-03 的任务行映射：族/插件字段、统一列、
// data/properties 分工与结算锚点。
func TestTaskRowMapping(t *testing.T) {
	submit := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	deadline := submit.Add(30 * 24 * time.Hour)
	origin := []string{"origin-mj-1"}

	code := 1
	row := TaskRow(Submission{
		PublicTaskID:      "task_internal_1",
		UpstreamTaskID:    "mj-id-1",
		Action:            ActionImagine,
		UserID:            7,
		TokenID:           11,
		Group:             "default",
		ChannelID:         3,
		LockedChannelID:   0,
		OriginTaskIDs:     origin,
		Prompt:            "a cat",
		State:             tasks.StateQueued,
		RawState:          "QUEUED",
		ProgressPercent:   45,
		Quota:             100,
		SubmitTime:        submit,
		RequestID:         "req-1",
		Code:              &code,
		Description:       "queued",
		RetentionDeadline: &deadline,
		Data:              map[string]any{"extra": "kept"},
	})

	if row.Family != tasks.FamilyMJ {
		t.Fatalf("Family = %q，期望 mj", row.Family)
	}
	if row.PluginKey != "" || row.PluginGeneration != 0 {
		t.Fatalf("family=mj 不得绑定插件: key=%q gen=%d", row.PluginKey, row.PluginGeneration)
	}
	if row.FetchMode != FetchModePoll {
		t.Fatalf("FetchMode = %q，期望 poll", row.FetchMode)
	}
	if row.UpstreamTaskID != "mj-id-1" || row.PublicTaskID != "task_internal_1" {
		t.Fatalf("id 映射错误: upstream=%q public=%q", row.UpstreamTaskID, row.PublicTaskID)
	}
	if row.Action != string(ActionImagine) {
		t.Fatalf("Action = %q", row.Action)
	}
	if row.State != tasks.StateQueued || row.RawState != "QUEUED" || row.ProgressPercent != 45 {
		t.Fatalf("状态列 = (%q,%q,%d)", row.State, row.RawState, row.ProgressPercent)
	}
	if row.UserID != 7 || row.Group != "default" || row.ChannelID != 3 || row.Quota != 100 {
		t.Fatalf("归属/结算列错误: %+v", row)
	}
	if row.SubmitTime == nil || !row.SubmitTime.Equal(submit) {
		t.Fatalf("SubmitTime = %v", row.SubmitTime)
	}
	if row.RetentionDeadline == nil || !row.RetentionDeadline.Equal(deadline) {
		t.Fatalf("RetentionDeadline = %v", row.RetentionDeadline)
	}
	if row.Properties[PropertyPrompt] != "a cat" {
		t.Fatalf("properties.prompt = %v", row.Properties[PropertyPrompt])
	}
	if row.Data[DataCode] != 1 || row.Data[DataDescription] != "queued" {
		t.Fatalf("data 快照错误: %#v", row.Data)
	}
	if row.Data["extra"] != "kept" {
		t.Fatalf("调用方 data 未保留: %#v", row.Data)
	}
	if row.PrivateData[PrivateRequestID] != "req-1" || row.PrivateData[PrivateTokenID] != int64(11) {
		t.Fatalf("结算锚点错误: %#v", row.PrivateData)
	}
	// 二进制载荷（base64/蒙版）绝不允许进入 properties/data。
	for _, forbidden := range []string{"base64Array", "maskBase64", "sourceBase64", "targetBase64"} {
		if _, ok := row.Properties[forbidden]; ok {
			t.Fatalf("properties 不得含 %s", forbidden)
		}
		if _, ok := row.Data[forbidden]; ok {
			t.Fatalf("data 不得含 %s", forbidden)
		}
	}
}

// TestTaskRowCopiesInputs 任务行不得与调用方共享切片/map（并发下的数据竞争）。
func TestTaskRowCopiesInputs(t *testing.T) {
	origin := []string{"a"}
	data := map[string]any{DataCode: 1}
	props := map[string]any{"k": "v"}
	row := TaskRow(Submission{
		PublicTaskID: "task_1", UpstreamTaskID: "mj-1", Action: ActionImagine,
		OriginTaskIDs: origin, Data: data, Properties: props, Prompt: "p",
	})
	origin[0] = "mutated"
	data[DataCode] = 99
	props["k"] = "mutated"

	if row.OriginTaskIDs[0] != "a" {
		t.Fatalf("OriginTaskIDs 与入参共享底层数组")
	}
	if row.Data[DataCode] != 1 {
		t.Fatalf("Data 与入参共享 map")
	}
	if row.Properties["k"] != "v" {
		t.Fatalf("Properties 与入参共享 map")
	}
}

// TestTaskRowClampsProgress 进度钳制到 DB CHECK 的 0–100。
func TestTaskRowClampsProgress(t *testing.T) {
	for _, c := range []struct{ in, want int }{{-5, 0}, {0, 0}, {45, 45}, {100, 100}, {150, 100}} {
		row := TaskRow(Submission{ProgressPercent: c.in})
		if row.ProgressPercent != c.want {
			t.Fatalf("进度 %d 钳制为 %d，期望 %d", c.in, row.ProgressPercent, c.want)
		}
	}
}

// TestUploadInlineTask 钉死 R-MJ-14 的 inline 终态行语义。
func TestUploadInlineTask(t *testing.T) {
	urls := []string{"https://cdn.example/a.png", "https://cdn.example/b.png"}
	row := UploadInlineTask(UploadSubmission{
		PublicTaskID: "task_upload_1",
		UserID:       7,
		TokenID:      11,
		Group:        "default",
		ChannelID:    3,
		Prompt:       "upload",
		Quota:        5,
		SubmitTime:   time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
		RequestID:    "req-upload",
		UploadURLs:   urls,
		Data:         map[string]any{"extra": "kept"},
	})

	if row.UpstreamTaskID != "" {
		t.Fatalf("上传行不得伪造 MjId，got %q", row.UpstreamTaskID)
	}
	if row.Family != tasks.FamilyMJ || row.Action != string(ActionUpload) {
		t.Fatalf("族/动作 = (%q,%q)", row.Family, row.Action)
	}
	if row.State != tasks.StateSuccess || row.ProgressPercent != 100 {
		t.Fatalf("上传行必须即时终态: state=%q progress=%d", row.State, row.ProgressPercent)
	}
	if row.FetchMode != FetchModeInline {
		t.Fatalf("FetchMode = %q，期望 inline", row.FetchMode)
	}
	if row.PluginKey != "" {
		t.Fatalf("plugin_key 必须为空")
	}
	got, ok := row.Data[DataUploadURLs].([]string)
	if !ok || len(got) != 2 || got[0] != urls[0] || got[1] != urls[1] {
		t.Fatalf("data.upload_urls = %#v", row.Data[DataUploadURLs])
	}
	if row.Data["extra"] != "kept" {
		t.Fatalf("调用方 data 未保留: %#v", row.Data)
	}
	if row.PrivateData[PrivateRequestID] != "req-upload" || row.PrivateData[PrivateTokenID] != int64(11) {
		t.Fatalf("结算锚点错误: %#v", row.PrivateData)
	}

	// 上传 URL 切片必须复制，不能被调用方后续修改影响。
	urls[0] = "mutated"
	if row.Data[DataUploadURLs].([]string)[0] != "https://cdn.example/a.png" {
		t.Fatalf("upload_urls 与入参共享底层数组")
	}

	// 空结果也必须是数组（result:[] 语义，审计可查）。
	empty := UploadInlineTask(UploadSubmission{PublicTaskID: "task_u2"})
	if emptyURLs, ok := empty.Data[DataUploadURLs].([]string); !ok || len(emptyURLs) != 0 {
		t.Fatalf("空上传结果必须是空数组，got %#v", empty.Data[DataUploadURLs])
	}
}
