package store

import "testing"

func TestPublicChatModelNamesFiltersDisabledAndNonChat(t *testing.T) {
	// 输入刻意逆序：输出必须自己排序，而不是顺带沿用输入顺序。
	names := []string{
		"f/mixed-endpoints", "e/no-endpoints", "d/chat",
		"c/no-meta", "b/embeddings-only", "a/disabled-exact",
	}
	meta := []ModelMeta{
		{ModelName: "a/disabled-exact", Status: 2, NameRule: 0},
		{ModelName: "b/embeddings-only", Status: 1, NameRule: 0,
			Endpoints: []byte(`{"/v1/embeddings":["POST"]}`)},
		{ModelName: "d/chat", Status: 1, NameRule: 0,
			Endpoints: []byte(`{"/v1/chat/completions":["POST"]}`)},
		{ModelName: "e/no-endpoints", Status: 1, NameRule: 0, Endpoints: nil},
		// 同时声明对话与非对话端点：只要命中其一就必须保留。
		{ModelName: "f/mixed-endpoints", Status: 1, NameRule: 0,
			Endpoints: []byte(`{"/v1/chat/completions":["POST"],"/v1/embeddings":["POST"]}`)},
	}
	got := publicChatModelNames(names, meta)
	want := []string{"c/no-meta", "d/chat", "e/no-endpoints", "f/mixed-endpoints"}
	if len(got) != len(want) {
		t.Fatalf("chat models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chat models = %v, want %v", got, want)
		}
	}
}
