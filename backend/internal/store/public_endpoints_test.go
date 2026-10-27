package store

import (
	"encoding/json"
	"testing"
)

func TestDeriveEndpointTypes(t *testing.T) {
	raw, _ := json.Marshal(map[string][]string{
		"/v1/chat/completions":                         {"POST"},
		"/v1/embeddings":                               {"POST"},
		"/v1/audio/transcriptions":                     {"POST"},
		"/v1beta/models/{model}:generateContent":       {"POST"},
		"/v1beta/models/{model}:streamGenerateContent": {"POST"}, // 未知，忽略
		"/custom/private":                              {"POST"}, // 未知，忽略
	})
	want := []string{"chat.completions", "embeddings", "audio.transcriptions", "generateContent"}
	got := DeriveEndpointTypes(raw)
	if len(got) != len(want) {
		t.Fatalf("endpoint 派生 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoint 派生 = %v，期望 %v", got, want)
		}
	}
	if got := DeriveEndpointTypes(nil); got == nil || len(got) != 0 {
		t.Fatalf("无端点必须是空数组，得到 %v", got)
	}
}
