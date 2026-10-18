package httpapi

// 公开的客户端粗粒度地理区域端点（GET /api/geoip，platform-03 §9.5）。
//
// 契约（api/openapi/paths/public.yaml:30-71 / schemas/public.yaml 的
// GeoIPResponse）：
//   - 公开、无需认证、恒 200；
//   - 只返回 country/region/source，不返回坐标、城市级精度、IP 原文、ASN
//     或代理链；
//   - 本地库缺失/损坏时 source=unknown（不返回 5xx）；
//   - Cache-Control: private, max-age=300（按客户端缓存）。
//
// M1 未引入 GeoIP 数据库：默认实现诚实返回 {country:null, region:null,
// source:"unknown"}。GeoIPLookup 接缝留给后续里程碑接入本地库，绝不在没有
// 数据时伪造国家。

import (
	"net/http"
)

// GeoIPResponse 是端点响应投影。指针字段表达「无法判定 = null」，与空串
// 的国家码可区分。
type GeoIPResponse struct {
	Country *string `json:"country"`
	Region  *string `json:"region"`
	Source  string  `json:"source"`
}

// GeoIPLookup 是本地 GeoIP 库的查询接缝。ip 只用于查询，绝不回显。
// source 取值必须是 "local_db" 或 "unknown"（契约 enum）。
type GeoIPLookup func(ip string) (country, region *string, source string)

func (h *authHandlers) handleGeoIP(w http.ResponseWriter, r *http.Request) {
	resp := GeoIPResponse{Source: "unknown"}
	if h.geoIP != nil {
		country, region, source := h.geoIP(clientIP(r))
		if source != "local_db" {
			// 未命中/库损坏一律归为 unknown；不把内部错误暴露为 5xx。
			source = "unknown"
			country, region = nil, nil
		}
		resp.Country, resp.Region, resp.Source = country, region, source
	}
	// 在 noStoreAPI 的默认 no-store 之后显式覆盖：地区信息允许按客户端缓存
	// 5 分钟；这是契约冻结的缓存语义。Pragma 必须一并清除，否则 HTTP/1.0
	// 代理仍会按 no-cache 处理，覆盖等于无效。
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Del("Pragma")
	writeJSON(w, http.StatusOK, resp)
}
