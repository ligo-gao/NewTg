package tasks

// W8/MJ-a2（M6）：SQL 使用的 MJ 渠道类型集合必须与 contract 的冻结类型一致。
//
// 这条断言防止「SQL 里改了字面量、领域层/契约没改」的漂移：渠道明明存在，
// 却因为集合不匹配永远选不中（表现为 no_available_channel）。

import (
	"testing"

	"newtg/gateway/internal/contract"
)

func TestMJChannelTypesMatchContract(t *testing.T) {
	want := map[string]bool{
		string(contract.ChannelTypeMidjourney):     true,
		string(contract.ChannelTypeMidjourneyPlus): true,
	}
	if len(mjChannelTypes) != len(want) {
		t.Fatalf("mjChannelTypes = %v, want %v", mjChannelTypes, want)
	}
	for _, typ := range mjChannelTypes {
		if !want[typ] {
			t.Fatalf("mjChannelTypes contains %q, which is not a contract MJ channel type", typ)
		}
	}
}
