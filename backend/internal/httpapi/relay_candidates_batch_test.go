package httpapi

// 本文件固定 P2-2 的候选渠道批量读取：跨协议过滤要在一次 SQL 里取回全部
// 候选的渠道行，而不是逐候选点查。语义（pin 失败上抛、非 pin 缺失跳过、
// 协议不匹配过滤）必须与逐条 Get 完全一致。

import (
	"context"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/crossproto"
	"newtg/gateway/internal/store"
)

// batchRelayChannelStore 在 fakeRelayChannelStore 之上实现批量读取，并
// 记录 Get/GetMany 的调用次数。
type batchRelayChannelStore struct {
	fakeRelayChannelStore
	missing  map[int64]bool
	batchErr error
	getMany  int
	lastIDs  []int64
}

func (f *batchRelayChannelStore) GetMany(_ context.Context, ids []int64) (map[int64]*store.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getMany++
	f.lastIDs = append([]int64(nil), ids...)
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[int64]*store.Channel, len(ids))
	for _, id := range ids {
		if f.missing[id] {
			continue
		}
		if ch, ok := f.channels[id]; ok {
			cp := *ch
			out[id] = &cp
		}
	}
	return out, nil
}

func batchCandidates(ids ...int64) []contract.ChannelCandidate {
	out := make([]contract.ChannelCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, contract.ChannelCandidate{ChannelID: int(id)})
	}
	return out
}

func TestFilterCandidatesByProtocolBatchesChannelReads(t *testing.T) {
	channels := &batchRelayChannelStore{
		fakeRelayChannelStore: fakeRelayChannelStore{channels: map[int64]*store.Channel{
			1: {ID: 1, Type: "openai"},
			2: {ID: 2, Type: "anthropic"},
			3: {ID: 3, Type: "openai"},
		}},
		missing: map[int64]bool{9: true},
	}
	candidates := batchCandidates(1, 2, 3, 9)

	got, err := filterCandidatesByProtocol(context.Background(), channels,
		candidates, crossproto.ProtocolAnthropic, false)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if channels.getMany != 1 || channels.getCalls != 0 {
		t.Fatalf("GetMany/Get = %d/%d，期望 1/0（候选必须一次批量读取）",
			channels.getMany, channels.getCalls)
	}
	if len(got) != 1 || got[0].ChannelID != 2 {
		t.Fatalf("过滤结果 = %+v，期望只剩渠道 2", got)
	}
	if len(channels.lastIDs) != 4 {
		t.Fatalf("批量读取 id 数 = %d，期望 4（含已删渠道，缺行由调用方按缺失处理）",
			len(channels.lastIDs))
	}
}

func TestFilterCandidatesByProtocolBatchKeepsPinnedSemantics(t *testing.T) {
	channels := &batchRelayChannelStore{
		fakeRelayChannelStore: fakeRelayChannelStore{channels: map[int64]*store.Channel{
			1: {ID: 1, Type: "openai"},
		}},
	}

	// pin 指向已删渠道：必须上抛，不能被批量读取吞成「协议不支持」。
	pinned := contract.ChannelCandidate{ChannelID: 7, Constraints: contract.ChannelConstraints{PinChannelID: 7}}
	if _, err := filterCandidatesByProtocol(context.Background(), channels,
		[]contract.ChannelCandidate{pinned}, crossproto.ProtocolAnthropic, false); err == nil {
		t.Fatal("pin 候选缺失时必须上抛错误")
	}
}

func TestFilterCandidatesByProtocolFallsBackWhenBatchUnsupported(t *testing.T) {
	channels := &batchRelayChannelStore{
		fakeRelayChannelStore: fakeRelayChannelStore{channels: map[int64]*store.Channel{
			1: {ID: 1, Type: "openai"},
		}},
	}
	// 用内嵌的 fake（没有 GetMany）验证回退路径仍逐条读取。
	got, err := filterCandidatesByProtocol(context.Background(), &channels.fakeRelayChannelStore,
		batchCandidates(1), crossproto.ProtocolOpenAI, false)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if channels.fakeRelayChannelStore.getCalls != 1 {
		t.Fatalf("回退路径 Get = %d，期望 1", channels.fakeRelayChannelStore.getCalls)
	}
	if len(got) != 1 {
		t.Fatalf("过滤结果 = %+v，期望 1 条", got)
	}
}
