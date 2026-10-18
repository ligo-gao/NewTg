package tokenstore

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
)

var (
	keyFormatRe  = regexp.MustCompile(`^nkt-[0-9A-Za-z]{48}$`)
	hashFormatRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// TestGenerateKeyFormatAndMask 覆盖 brief (a) 的格式/哈希/掩码部分：
// nkt- + 48 位 Base62；key_hash=SHA256(完整明文)；掩码恒为 nkt-+24×•。
func TestGenerateKeyFormatAndMask(t *testing.T) {
	plain, keyHash, masked, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !keyFormatRe.MatchString(plain) {
		t.Fatalf("明文 = %q，期望 nkt- + 48 位 Base62", plain)
	}
	if !hashFormatRe.MatchString(keyHash) {
		t.Fatalf("key_hash = %q，期望 64 位小写十六进制", keyHash)
	}
	if keyHash != auth.HashTokenKey(plain) {
		t.Fatalf("key_hash 不是完整明文（含 nkt- 前缀）的 SHA-256：got=%s want=%s", keyHash, auth.HashTokenKey(plain))
	}
	if want := "nkt-" + strings.Repeat("•", 24); masked != want {
		t.Fatalf("masked = %q，期望 %q", masked, want)
	}
}

// TestGenerateKeyUnique 覆盖 brief (a)：连续生成 100 枚不得重复。
func TestGenerateKeyUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	seenHash := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		plain, hash, _, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey #%d: %v", i, err)
		}
		if _, dup := seen[plain]; dup {
			t.Fatalf("第 %d 枚明文重复: %q", i, plain)
		}
		if _, dup := seenHash[hash]; dup {
			t.Fatalf("第 %d 枚哈希重复", i)
		}
		seen[plain] = struct{}{}
		seenHash[hash] = struct{}{}
	}
}

// TestValidateAutoGroups 覆盖 brief (b)(c) 的纯校验部分：
// 空列表拒绝（I-20）、重复拒绝、越界拒绝、恰好 20 接受、21 拒绝。
func TestValidateAutoGroups(t *testing.T) {
	allowed := map[string]struct{}{}
	for _, g := range []string{"g1", "g2", "g3", "g4", "g5", "g6", "g7", "g8", "g9", "g10",
		"g11", "g12", "g13", "g14", "g15", "g16", "g17", "g18", "g19", "g20", "g21"} {
		allowed[g] = struct{}{}
	}
	twenty := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		twenty = append(twenty, "g"+itoa(i))
	}
	twentyOne := append(append([]string{}, twenty...), "g21")

	cases := []struct {
		name    string
		list    []string
		max     int
		wantErr string // 空串 = 期望通过
	}{
		{name: "空列表拒绝（I-20）", list: nil, max: 20, wantErr: "至少"},
		{name: "重复项拒绝", list: []string{"g1", "g1"}, max: 20, wantErr: "重复"},
		{name: "候选集外拒绝", list: []string{"g1", "gX"}, max: 20, wantErr: "候选"},
		{name: "恰好 20 接受", list: twenty, max: 20},
		{name: "21 项拒绝", list: twentyOne, max: 20, wantErr: "最多"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAutoGroups(tc.list, allowed, tc.max)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望 %q 类错误，实际通过", tc.wantErr)
			}
			if err.Field != "auto_groups" {
				t.Errorf("Field = %q，期望 auto_groups", err.Field)
			}
			if !strings.Contains(err.Message, tc.wantErr) {
				t.Errorf("Message = %q，期望包含 %q", err.Message, tc.wantErr)
			}
		})
	}
}

// TestValidateAutoGroupsBoundaryWithMax 覆盖 TC-F-02-13-08 的动态上限：
// 上限来自配置，恰好 max 接受、max+1 拒绝。
func TestValidateAutoGroupsBoundaryWithMax(t *testing.T) {
	allowed := map[string]struct{}{"a": {}, "b": {}, "c": {}}
	if err := validateAutoGroups([]string{"a", "b"}, allowed, 2); err != nil {
		t.Fatalf("恰好等于上限应接受: %v", err)
	}
	if err := validateAutoGroups([]string{"a", "b", "c"}, allowed, 2); err == nil {
		t.Fatal("超过上限应拒绝")
	}
}

// TestDeriveStatus 冻结状态优先级 disabled > expired > exhausted > enabled。
func TestDeriveStatus(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	zero := int64(0)
	positive := int64(10)

	cases := []struct {
		name     string
		raw      string
		expires  *time.Time
		remain   *int64
		expected string
	}{
		{"enabled", StatusEnabled, nil, nil, StatusEnabled},
		{"disabled 优先于过期", StatusDisabled, &past, &zero, StatusDisabled},
		{"expired 优先于 exhausted", StatusEnabled, &past, &zero, StatusExpired},
		{"exhausted", StatusEnabled, nil, &zero, StatusExhausted},
		{"positive remain", StatusEnabled, &future, &positive, StatusEnabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveStatus(tc.raw, tc.expires, tc.remain, now); got != tc.expected {
				t.Fatalf("deriveStatus = %q，期望 %q", got, tc.expected)
			}
		})
	}
}

// TestNormalizeGroupCoupling 冻结 I-23：非 auto 分组时 auto_groups 清空、
// cross_group_retry 强制为假；auto 分组保留提交值。
func TestNormalizeGroupCoupling(t *testing.T) {
	auto, retry := normalizeGroupCoupling("default", []string{"g1"}, true)
	if len(auto) != 0 || retry {
		t.Fatalf("非 auto 分组必须清空候选集并置假：auto=%v retry=%v", auto, retry)
	}
	auto, retry = normalizeGroupCoupling("auto", []string{"g1", "g2"}, true)
	if len(auto) != 2 || !retry {
		t.Fatalf("auto 分组应保留提交值：auto=%v retry=%v", auto, retry)
	}
}

// TestConfigDefaults 覆盖 settings 缺省/自定义值：
// token.max_auto_groups_per_token 默认 20；relay.cross_group_retry_max_groups 默认 3；
// token.auto_groups 缺省为空。
func TestConfigDefaults(t *testing.T) {
	var nilSettings mapSettings
	if got := maxAutoGroups(nilSettings); got != DefaultMaxAutoGroups {
		t.Fatalf("默认上限 = %d，期望 %d", got, DefaultMaxAutoGroups)
	}
	if got := crossGroupRetryMax(nilSettings); got != DefaultCrossGroupRetryMaxGroups {
		t.Fatalf("默认跨组上限 = %d，期望 %d", got, DefaultCrossGroupRetryMaxGroups)
	}
	if got := siteAutoGroups(nilSettings); len(got) != 0 {
		t.Fatalf("默认站点自动分组 = %v，期望空", got)
	}

	settings := mapSettings{
		KeyAutoGroups:         json.RawMessage(`["pro","default"]`),
		KeyMaxAutoGroups:      json.RawMessage(`5`),
		KeyCrossGroupRetryMax: json.RawMessage(`4`),
	}
	if got := maxAutoGroups(settings); got != 5 {
		t.Fatalf("自定义上限 = %d，期望 5", got)
	}
	if got := crossGroupRetryMax(settings); got != 4 {
		t.Fatalf("自定义跨组上限 = %d，期望 4", got)
	}
	groups := siteAutoGroups(settings)
	if len(groups) != 2 || groups[0] != "pro" || groups[1] != "default" {
		t.Fatalf("站点自动分组 = %v，期望 [pro default]", groups)
	}
}

// TestConfigMalformedFallsBack：损坏的配置值不得让令牌接口 500，回退默认值。
func TestConfigMalformedFallsBack(t *testing.T) {
	settings := mapSettings{
		KeyAutoGroups:         json.RawMessage(`{`),
		KeyMaxAutoGroups:      json.RawMessage(`"not-a-number"`),
		KeyCrossGroupRetryMax: json.RawMessage(`-1`),
	}
	if got := maxAutoGroups(settings); got != DefaultMaxAutoGroups {
		t.Fatalf("损坏上限回退 = %d，期望 %d", got, DefaultMaxAutoGroups)
	}
	if got := crossGroupRetryMax(settings); got != DefaultCrossGroupRetryMaxGroups {
		t.Fatalf("负跨组上限回退 = %d，期望 %d", got, DefaultCrossGroupRetryMaxGroups)
	}
	if got := siteAutoGroups(settings); len(got) != 0 {
		t.Fatalf("损坏白名单回退 = %v，期望空", got)
	}
}

// TestValidateNameAndIPs 覆盖创建校验的其余字段。
func TestValidateNameAndIPs(t *testing.T) {
	if _, err := normalizeName("  prod-key  "); err != nil {
		t.Fatalf("合法名称被拒: %v", err)
	}
	if got, _ := normalizeName("  prod-key  "); got != "prod-key" {
		t.Fatalf("名称未裁剪: %q", got)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 65)} {
		if _, err := normalizeName(bad); err == nil {
			t.Fatalf("非法名称 %q 应被拒", bad)
		}
	}
	if ips, err := normalizeIPs(nil); err != nil || ips != nil {
		t.Fatalf("nil IP 白名单应通过并保持 nil：%v %v", ips, err)
	}
	if _, err := normalizeIPs([]string{"10.0.0.0/8", "2001:db8::/32", "192.168.1.1"}); err != nil {
		t.Fatalf("合法 CIDR/IP 被拒: %v", err)
	}
	if _, err := normalizeIPs([]string{"not-an-ip"}); err == nil {
		t.Fatal("非法 IP 应被拒")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// mapSettings 是 Settings 缝隙的内存实现（单测与集成测试共用）。
type mapSettings map[string]json.RawMessage

func (m mapSettings) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}
