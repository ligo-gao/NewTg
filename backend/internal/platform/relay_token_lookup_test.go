package platform

import (
	"context"
	"errors"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/tokenstore"
)

// TestRelayTokenLookupPrefersCachedStore 固定 P2-2 的接线契约：生产装配
// 必须把带缓存的 tokenstore.Store 交给中继，而不是每请求直查 DB 的
// store.TokenRepo；只有探针/测试装配（assembly 为空或 Tokens 未构造）才
// 回退到无缓存实现。
func TestRelayTokenLookupPrefersCachedStore(t *testing.T) {
	fallback := stubLookup{}
	cached := tokenstore.New(nil, nil, nil)

	if got := relayTokenLookup(&gatewayAssembly{Tokens: cached}, fallback); got != auth.TokenLookup(cached) {
		t.Fatalf("装配了 tokenstore 时必须使用缓存实现，实际 %T", got)
	}
	if got := relayTokenLookup(&gatewayAssembly{}, fallback); got != auth.TokenLookup(fallback) {
		t.Fatalf("未装配 tokenstore 时必须回退到传入实现，实际 %T", got)
	}
	if got := relayTokenLookup(nil, fallback); got != auth.TokenLookup(fallback) {
		t.Fatalf("assembly 为 nil 时必须回退到传入实现，实际 %T", got)
	}
}

type stubLookup struct{}

func (stubLookup) Resolve(context.Context, string) (*auth.TokenMeta, error) {
	return nil, errors.New("stub")
}
