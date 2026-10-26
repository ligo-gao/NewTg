package pricing

import (
	"testing"

	"newtg/gateway/internal/contract"
)

// TestCacheTiersWithTimeMultiplier 锁定「缓存分档 × 峰谷倍率」的实扣数字，
// 与具体厂商无关：任何模型只要有未命中 / 命中 / 输出三档价并开启时段倍率，
// 扣费都必须走同一条链路。未命中 200、命中 4、输出 800（积分/1M），
// P=128k、CR=64k、C=4k。峰值 29.056 → 四舍五入 29；空闲 ×0.5 = 14.528 → 15。
// 缓存写入 10000 tokens、单价 0：不得产生费用。
func TestCacheTiersWithTimeMultiplier(t *testing.T) {
	snap := ratioSnapshot(200, 800, 4, 0)
	params := contract.TokenParams{
		P: 128000, CR: 64000, C: 4000, CC5m: 10000, ImageCount: ImageCountUnset,
	}
	peakQuota, err := ComputeQuota(snap, params, nil)
	if err != nil {
		t.Fatalf("ComputeQuota(peak): %v", err)
	}
	if peakQuota != 29 {
		t.Fatalf("peak quota = %d, want 29", peakQuota)
	}

	off, err := ApplyTimeMultiplier(snap, 0.5, "", "")
	if err != nil {
		t.Fatalf("ApplyTimeMultiplier: %v", err)
	}
	offQuota, err := ComputeQuota(off, params, nil)
	if err != nil {
		t.Fatalf("ComputeQuota(off-peak): %v", err)
	}
	if offQuota != 15 {
		t.Fatalf("off-peak quota = %d, want 15", offQuota)
	}
}
