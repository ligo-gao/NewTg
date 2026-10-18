package adapters

import (
	"errors"
	"reflect"
	"testing"

	"newtg/gateway/internal/contract"
)

// TestRegistryDispatchTableInvariants 覆盖 relay-02 §2.3 的启动期不变量：
// ① 每个具名类型都有条目（AC-B-01）；② AdapterKey 非空；③ FallbackTo 指向已注册键；
// ④ ForbidFallback 与 FallbackTo 互斥；⑤ 哨兵不参与选择。
func TestRegistryDispatchTableInvariants(t *testing.T) {
	if got, want := len(DispatchTable), len(contract.ChannelTypes); got != want {
		t.Fatalf("DispatchTable 条目 = %d, want %d（59 个具名类型）", got, want)
	}
	if _, ok := DispatchTable[contract.ChannelTypeUnknown]; ok {
		t.Fatal("哨兵类型不得出现在 DispatchTable（不参与选择，relay-02 §2.3）")
	}
	for _, ct := range contract.ChannelTypes {
		entry, ok := DispatchTable[ct]
		if !ok {
			t.Fatalf("渠道类型 %q 缺少分派条目", ct)
		}
		if entry.AdapterKey == "" {
			t.Fatalf("渠道类型 %q 的 AdapterKey 为空", ct)
		}
		if entry.ForbidFallback && entry.FallbackTo != "" {
			t.Fatalf("渠道类型 %q 同时声明 ForbidFallback 与 FallbackTo", ct)
		}
		if entry.FallbackTo != "" {
			key := contract.AdapterKey(entry.FallbackTo)
			if _, ok := adapterConstructors[key]; !ok {
				t.Fatalf("渠道类型 %q 的 FallbackTo %q 不是已注册键（声明矛盾，须启动期失败）", ct, entry.FallbackTo)
			}
		}
	}
	// 不变量校验函数本身必须能通过（init 已执行一次，这里显式再跑一遍）。
	validateDispatchTable(DispatchTable)
}

// TestLookupAllNamedTypes 断言 59 个具名类型全部命中（R-P6：21/61 也必须有条目）。
func TestLookupAllNamedTypes(t *testing.T) {
	for _, ct := range contract.ChannelTypes {
		ctor, ok := Lookup(ct)
		if !ok {
			t.Fatalf("Lookup(%q) 未命中", ct)
		}
		if ctor == nil {
			t.Fatalf("Lookup(%q) 返回 nil 构造器", ct)
		}
		if a := ctor(); a == nil {
			t.Fatalf("Lookup(%q) 的构造器返回 nil adapter", ct)
		}
	}
}

func TestLookupSentinelAndUnknownMiss(t *testing.T) {
	for _, ct := range []contract.ChannelType{contract.ChannelTypeUnknown, "not-a-channel-type"} {
		if ctor, ok := Lookup(ct); ok || ctor != nil {
			t.Fatalf("Lookup(%q) = %v, true；哨兵/未知类型必须未命中", ct, ctor)
		}
	}
}

// TestDispatchUnsupportedTypes 断言不可用类型由 Dispatch（而非 Lookup）判定失败：
// 21/61 显式失败（R-P6），未实现的键不得隐式回退（relay-02 §2.3）。
func TestDispatchUnsupportedTypes(t *testing.T) {
	for _, ct := range []contract.ChannelType{
		contract.ChannelTypeAIProxyLibrary, // 21：无适配器，ForbidFallback
		contract.ChannelTypeTaskPlugin,     // 61：禁止回退
		contract.ChannelTypeSora,           // AKTask 尚未实现，且无回退声明
		contract.ChannelTypeMoonshot,       // AKAnthropicNative 尚未实现，且无回退声明
		contract.ChannelTypeUnknown,        // 哨兵
		"not-a-channel-type",
	} {
		a, err := Dispatch(ct)
		if err == nil {
			t.Fatalf("Dispatch(%q) 未失败，返回 %T", ct, a)
		}
		if a != nil {
			t.Fatalf("Dispatch(%q) 返回了非 nil adapter（%T）；失败时不得返回适配器", ct, a)
		}
		if !errors.Is(err, ErrEndpointUnsupported) {
			t.Fatalf("Dispatch(%q) error = %v, want ErrEndpointUnsupported（contract.ErrEndpointUnsupported）", ct, err)
		}
		if class, ok := ClassOf(err); !ok || class != contract.ErrEndpointUnsupported {
			t.Fatalf("Dispatch(%q) 错误分类 = %v/%v, want ErrEndpointUnsupported", ct, class, ok)
		}
	}
}

func TestDispatchOpenAIAndAzure(t *testing.T) {
	for _, ct := range []contract.ChannelType{contract.ChannelTypeOpenAI, contract.ChannelTypeAzure} {
		a, err := Dispatch(ct)
		if err != nil {
			t.Fatalf("Dispatch(%q): %v", ct, err)
		}
		if a == nil {
			t.Fatalf("Dispatch(%q) 返回 nil adapter", ct)
		}
		if a.Type() != ct {
			t.Fatalf("Dispatch(%q).Type() = %q（Azure 变体须返回各自类型）", ct, a.Type())
		}
		if _, ok := a.(contract.StreamAdapter); !ok {
			t.Fatalf("Dispatch(%q) 的 adapter 未实现 contract.StreamAdapter", ct)
		}
	}
}

// TestRegistryMoonshotAndSora 覆盖 R-01 / R-04：
// moonshot → AKAnthropicNative；sora → AKTask 且只有 RoleTask（无 RoleChat）。
func TestRegistryMoonshotAndSora(t *testing.T) {
	if got := DispatchTable[contract.ChannelTypeMoonshot].AdapterKey; got != string(AKAnthropicNative) {
		t.Fatalf("moonshot AdapterKey = %q, want %q（R-01）", got, AKAnthropicNative)
	}
	if got := DispatchTable[contract.ChannelTypeSora].AdapterKey; got != string(AKTask) {
		t.Fatalf("sora AdapterKey = %q, want %q（R-04）", got, AKTask)
	}
	ctor, ok := Lookup(contract.ChannelTypeSora)
	if !ok {
		t.Fatal("Lookup(sora) 未命中")
	}
	roles := ctor().Caps().Roles
	if !reflect.DeepEqual(roles, []contract.ChannelRole{contract.RoleTask}) {
		t.Fatalf("sora Roles = %v, want [RoleTask]（不得含 RoleChat）", roles)
	}
	if got := NewOpenAI().Caps().Roles; !reflect.DeepEqual(got, []contract.ChannelRole{contract.RoleChat}) {
		t.Fatalf("OpenAI Caps().Roles = %v, want [RoleChat]", got)
	}
}

// TestRegistryFallbackDeclarations 断言基线遗留类型在表中显式写出回退（relay-02 §2.3 关键约束）。
func TestRegistryFallbackDeclarations(t *testing.T) {
	for _, ct := range []contract.ChannelType{
		contract.ChannelTypeOpenAIMax,
		contract.ChannelTypeAILS,
		contract.ChannelTypeAIProxy,
		contract.ChannelTypeAPI2GPT,
		contract.ChannelTypeAIGC2D,
	} {
		entry := DispatchTable[ct]
		if entry.FallbackTo != string(AKOpenAI) {
			t.Fatalf("%q FallbackTo = %q, want %q（逐类型显式声明，禁止 map 未命中的隐式默认）",
				ct, entry.FallbackTo, AKOpenAI)
		}
		if entry.ForbidFallback {
			t.Fatalf("%q 不应禁止回退", ct)
		}
	}
	for _, ct := range []contract.ChannelType{contract.ChannelTypeAIProxyLibrary, contract.ChannelTypeTaskPlugin} {
		entry := DispatchTable[ct]
		if !entry.ForbidFallback || entry.FallbackTo != "" {
			t.Fatalf("%q = %+v, want ForbidFallback=true 且 FallbackTo 为空", ct, entry)
		}
	}
}

// TestRegistryAdapterKeysUnique 断言所有条目引用的 AdapterKey 都来自 §2.5 的键全集，
// 且 59 行数据里不存在空键/未知键。
func TestRegistryAdapterKeysUnique(t *testing.T) {
	known := map[contract.AdapterKey]bool{
		AKOpenAI: true, AKAnthropicNative: true, AKGeminiNative: true, AKVertex: true,
		AKPaLM: true, AKOllama: true, AKBaidu: true, AKZhipu: true, AKXunfei: true,
		AKTencent: true, AKAws: true, AKCohere: true, AKMiniMax: true, AKDify: true,
		AKJina: true, AKCloudflare: true, AKMokaAI: true, AKCoze: true, AKReplicate: true,
		AKCodex: true, AKAdvancedCustom: true, AKSub2API: true, AKNewAPI: true,
		AKTask: true, AKTaskPlugin: true, AKAIProxyLibrary: true,
	}
	seen := make(map[contract.ChannelType]bool, len(DispatchTable))
	for ct, entry := range DispatchTable {
		if seen[ct] {
			t.Fatalf("渠道类型 %q 重复", ct)
		}
		seen[ct] = true
		key := contract.AdapterKey(entry.AdapterKey)
		if !known[key] {
			t.Fatalf("渠道类型 %q 引用了 §2.5 之外的 AdapterKey %q", ct, entry.AdapterKey)
		}
	}
	if len(known) != 26 {
		t.Fatalf("§2.5 的 AdapterKey 数量 = %d, want 26", len(known))
	}
}
