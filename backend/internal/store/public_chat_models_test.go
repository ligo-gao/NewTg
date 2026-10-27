package store

import "testing"

func TestPublicChatModelNamesFiltersDisabledAndNonChat(t *testing.T) {
	names := []string{
		"a/disabled-exact", "b/embeddings-only", "c/no-meta", "d/chat", "e/no-endpoints",
	}
	meta := []ModelMeta{
		{ModelName: "a/disabled-exact", Status: 2, NameRule: 0},
		{ModelName: "b/embeddings-only", Status: 1, NameRule: 0,
			Endpoints: []byte(`{"/v1/embeddings":["POST"]}`)},
		{ModelName: "d/chat", Status: 1, NameRule: 0,
			Endpoints: []byte(`{"/v1/chat/completions":["POST"]}`)},
		{ModelName: "e/no-endpoints", Status: 1, NameRule: 0, Endpoints: nil},
	}
	got := publicChatModelNames(names, meta)
	want := []string{"c/no-meta", "d/chat", "e/no-endpoints"}
	if len(got) != len(want) {
		t.Fatalf("chat models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chat models = %v, want %v", got, want)
		}
	}
}
