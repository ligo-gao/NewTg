// provider 响应适配器。
//
// 这里是**唯一**把 io.net 原始 JSON 字段映射为内部归一化结构的地方：
// provider 真实响应形状未实测（R-M6D-13），实测后需要修正的字段名应集中
// 在本文件，而不是散落在 client/handler 里。
//
// 容忍策略：
//   - 响应可能是直接对象，也可能包在 {"data": ...} 里（参照侧两种都存在）；
//   - 时间可能是 RFC3339、无时区字符串或 unix 数字；
//   - 日志可能是纯文本，也可能是 {logs,next_cursor,has_more} JSON。
package ionet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// flexTime 容忍多种时间表示。零值在映射层转为 nil。
type flexTime struct{ time.Time }

func (t *flexTime) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] != '"' {
		seconds, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return fmt.Errorf("ionet: unsupported time value %q", trimmed)
		}
		t.Time = time.Unix(int64(seconds), 0).UTC()
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, text); err == nil {
			t.Time = parsed.UTC()
			return nil
		}
	}
	return fmt.Errorf("ionet: unsupported time layout %q", text)
}

func (t flexTime) ptr() *time.Time {
	if t.IsZero() {
		return nil
	}
	value := t.Time
	return &value
}

// unwrapData 在响应是 {"data": ...} 时返回内层值，否则原样返回。
func unwrapData(raw []byte) []byte {
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return raw
	}
	if len(probe.Data) == 0 {
		return raw
	}
	inner := bytes.TrimSpace(probe.Data)
	if len(inner) == 0 || string(inner) == "null" {
		return raw
	}
	return inner
}

// ---------- wire 类型 ----------

type wireDeployment struct {
	ID                      string   `json:"id"`
	Name                    string   `json:"name"`
	DeploymentName          string   `json:"deployment_name"`
	Status                  string   `json:"status"`
	CompletedPercent        float64  `json:"completed_percent"`
	HardwareQuantity        int      `json:"hardware_quantity"`
	BrandName               string   `json:"brand_name"`
	HardwareName            string   `json:"hardware_name"`
	ComputeMinutesServed    int      `json:"compute_minutes_served"`
	ComputeMinutesRemaining int      `json:"compute_minutes_remaining"`
	CreatedAt               flexTime `json:"created_at"`
}

type wireDeploymentList struct {
	Deployments []wireDeployment `json:"deployments"`
	Total       int              `json:"total"`
	Statuses    []string         `json:"statuses"`
}

type wireDeploymentDetail struct {
	ID                      string             `json:"id"`
	Status                  string             `json:"status"`
	CreatedAt               flexTime           `json:"created_at"`
	StartedAt               flexTime           `json:"started_at"`
	FinishedAt              flexTime           `json:"finished_at"`
	AmountPaid              float64            `json:"amount_paid"`
	CompletedPercent        float64            `json:"completed_percent"`
	TotalGPUs               int                `json:"total_gpus"`
	GPUsPerContainer        int                `json:"gpus_per_container"`
	TotalContainers         int                `json:"total_containers"`
	HardwareID              int                `json:"hardware_id"`
	HardwareName            string             `json:"hardware_name"`
	BrandName               string             `json:"brand_name"`
	Locations               []wireLocation     `json:"locations"`
	ComputeMinutesServed    int                `json:"compute_minutes_served"`
	ComputeMinutesRemaining int                `json:"compute_minutes_remaining"`
	ContainerConfig         *wireContainerConf `json:"container_config"`
}

type wireLocation struct {
	ID   int    `json:"id"`
	ISO2 string `json:"iso2"`
	Name string `json:"name"`
}

type wireContainerConf struct {
	Entrypoint   []string       `json:"entrypoint"`
	EnvVariables map[string]any `json:"env_variables"`
	TrafficPort  int            `json:"traffic_port"`
	ImageURL     string         `json:"image_url"`
}

type wireContainer struct {
	DeviceID         string        `json:"device_id"`
	ContainerID      string        `json:"container_id"`
	Hardware         string        `json:"hardware"`
	BrandName        string        `json:"brand_name"`
	CreatedAt        flexTime      `json:"created_at"`
	UptimePercent    int           `json:"uptime_percent"`
	GPUsPerContainer int           `json:"gpus_per_container"`
	Status           string        `json:"status"`
	ContainerEvents  []wireEvent   `json:"container_events"`
	PublicURL        string        `json:"public_url"`
	Events           []wireEvent   `json:"events"`
	Ports            []interface{} `json:"ports"`
}

type wireEvent struct {
	Time    flexTime `json:"time"`
	Message string   `json:"message"`
}

type wireContainerList struct {
	Total   int             `json:"total"`
	Workers []wireContainer `json:"workers"`
	Items   []wireContainer `json:"containers"`
}

type wireActionResponse struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"`
}

type wireRenameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type wireReplica struct {
	ID                int    `json:"id"`
	LocationID        int    `json:"location_id"`
	ISO2              string `json:"iso2"`
	Name              string `json:"name"`
	LocationName      string `json:"location_name"`
	AvailableReplicas int    `json:"available_replicas"`
	AvailableCount    int    `json:"available_count"`
	MaxGPUs           int    `json:"max_gpus"`
}

type wireLoggerEntry struct {
	Timestamp flexTime `json:"timestamp"`
	Level     string   `json:"level"`
	Message   string   `json:"message"`
	Line      string   `json:"line"`
}

// ---------- 映射函数 ----------

func decodeDeploymentPage(raw []byte) (DeploymentPage, error) {
	var wire wireDeploymentList
	if err := json.Unmarshal(unwrapData(raw), &wire); err != nil {
		return DeploymentPage{}, &InvalidResponseError{Err: err}
	}
	items := make([]DeploymentSummary, 0, len(wire.Deployments))
	for _, item := range wire.Deployments {
		name := item.Name
		if name == "" {
			name = item.DeploymentName
		}
		items = append(items, DeploymentSummary{
			ID:       item.ID,
			Name:     name,
			Status:   normalizeStatus(item.Status),
			GPUCount: item.HardwareQuantity,
			// ReplicaCount 与 GPUCount 都取 hardware_quantity：参照侧同样是
			// 1:1 推导，但 provider 真实语义未实测（R-M6D-13 待实测清单）。
			ReplicaCount:            item.HardwareQuantity,
			CompletedPercent:        item.CompletedPercent,
			HardwareName:            item.HardwareName,
			BrandName:               item.BrandName,
			ComputeMinutesServed:    item.ComputeMinutesServed,
			ComputeMinutesRemaining: item.ComputeMinutesRemaining,
			CreatedAt:               item.CreatedAt.Time,
		})
	}
	// Total 必须保留 provider 的原值：0 表示「provider 未返回总数」，不能
	// 伪造成本页条数。handler 的全量扫描依赖这个区分——伪造会让「只扫了
	// 第一页」被误判成「已经取全」（复核 HIGH-1）。
	return DeploymentPage{Items: items, Total: wire.Total, Statuses: wire.Statuses}, nil
}

func decodeDeploymentDetail(raw []byte, id string) (DeploymentDetail, error) {
	var wire wireDeploymentDetail
	if err := json.Unmarshal(unwrapData(raw), &wire); err != nil {
		return DeploymentDetail{}, &InvalidResponseError{Err: err}
	}
	detail := DeploymentDetail{
		ID:                      firstNonEmpty(wire.ID, id),
		Status:                  normalizeStatus(wire.Status),
		CreatedAt:               wire.CreatedAt.ptr(),
		StartedAt:               wire.StartedAt.ptr(),
		FinishedAt:              wire.FinishedAt.ptr(),
		AmountPaid:              wire.AmountPaid,
		CompletedPercent:        wire.CompletedPercent,
		TotalGPUs:               wire.TotalGPUs,
		GPUsPerContainer:        wire.GPUsPerContainer,
		TotalContainers:         wire.TotalContainers,
		HardwareID:              wire.HardwareID,
		HardwareName:            wire.HardwareName,
		BrandName:               wire.BrandName,
		ComputeMinutesServed:    wire.ComputeMinutesServed,
		ComputeMinutesRemaining: wire.ComputeMinutesRemaining,
	}
	for _, loc := range wire.Locations {
		detail.Locations = append(detail.Locations, DeploymentLocation{
			ID: loc.ID, ISO2: strings.ToUpper(strings.TrimSpace(loc.ISO2)), Name: loc.Name,
		})
	}
	if wire.ContainerConfig != nil {
		detail.ContainerConfig = DeploymentContainerConfig{
			Entrypoint:   wire.ContainerConfig.Entrypoint,
			EnvVariables: wire.ContainerConfig.EnvVariables,
			TrafficPort:  wire.ContainerConfig.TrafficPort,
			ImageURL:     wire.ContainerConfig.ImageURL,
		}
	}
	return detail, nil
}

func decodeContainerList(raw []byte) (ContainerList, error) {
	var wire wireContainerList
	if err := json.Unmarshal(unwrapData(raw), &wire); err != nil {
		return ContainerList{}, &InvalidResponseError{Err: err}
	}
	workers := wire.Workers
	if len(workers) == 0 && len(wire.Items) > 0 {
		workers = wire.Items
	}
	out := ContainerList{Total: wire.Total, Containers: make([]Container, 0, len(workers))}
	for _, item := range workers {
		out.Containers = append(out.Containers, mapContainer(item))
	}
	if out.Total == 0 {
		out.Total = len(out.Containers)
	}
	return out, nil
}

func decodeContainer(raw []byte) (Container, error) {
	var wire wireContainer
	if err := json.Unmarshal(unwrapData(raw), &wire); err != nil {
		return Container{}, &InvalidResponseError{Err: err}
	}
	return mapContainer(wire), nil
}

func mapContainer(wire wireContainer) Container {
	events := wire.ContainerEvents
	if len(events) == 0 {
		events = wire.Events
	}
	out := Container{
		DeviceID:         wire.DeviceID,
		ContainerID:      wire.ContainerID,
		Hardware:         wire.Hardware,
		BrandName:        wire.BrandName,
		Status:           normalizeStatus(wire.Status),
		GPUsPerContainer: wire.GPUsPerContainer,
		UptimePercent:    wire.UptimePercent,
		CreatedAt:        wire.CreatedAt.ptr(),
		PublicURL:        wire.PublicURL,
	}
	for _, event := range events {
		out.Events = append(out.Events, ContainerEvent{Time: event.Time.Time, Message: event.Message})
	}
	return out
}

func decodeLogsPage(raw []byte) (LogsPage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var envelope struct {
			Logs          json.RawMessage `json:"logs"`
			NextCursor    string          `json:"next_cursor"`
			NextPageToken string          `json:"next_page_token"`
			HasMore       bool            `json:"has_more"`
		}
		if err := json.Unmarshal(trimmed, &envelope); err == nil && len(envelope.Logs) > 0 {
			var lines []string
			if err := json.Unmarshal(envelope.Logs, &lines); err != nil {
				var entries []wireLoggerEntry
				if err := json.Unmarshal(envelope.Logs, &entries); err != nil {
					return LogsPage{}, &InvalidResponseError{Err: err}
				}
				for _, entry := range entries {
					line := entry.Message
					if line == "" {
						line = entry.Line
					}
					lines = append(lines, line)
				}
			}
			token := firstNonEmpty(envelope.NextPageToken, envelope.NextCursor)
			return LogsPage{Lines: lines, NextPageToken: token, HasMore: envelope.HasMore || token != ""}, nil
		}
	}
	// 纯文本：按行拆分，丢弃空行（与参照侧一致）。
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := make([]string, 0)
	for _, line := range strings.Split(normalized, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return LogsPage{Lines: lines}, nil
}

func decodeAvailableReplicas(raw []byte, hardwareID, gpuCount int) (AvailableReplicas, error) {
	inner := unwrapData(raw)
	var list []wireReplica
	if err := json.Unmarshal(inner, &list); err != nil {
		var wrapped struct {
			Replicas []wireReplica `json:"replicas"`
		}
		if err := json.Unmarshal(inner, &wrapped); err != nil {
			return AvailableReplicas{}, &InvalidResponseError{Err: err}
		}
		list = wrapped.Replicas
	}
	out := AvailableReplicas{Replicas: make([]AvailableReplica, 0, len(list))}
	for _, item := range list {
		locationID := item.LocationID
		if locationID == 0 {
			locationID = item.ID
		}
		count := item.AvailableReplicas
		if count == 0 {
			count = item.AvailableCount
		}
		maxGPUs := item.MaxGPUs
		if maxGPUs == 0 {
			maxGPUs = gpuCount
		}
		name := item.LocationName
		if name == "" {
			name = item.Name
		}
		out.Replicas = append(out.Replicas, AvailableReplica{
			LocationID:     locationID,
			LocationName:   name,
			HardwareID:     hardwareID,
			AvailableCount: count,
			MaxGPUs:        maxGPUs,
		})
	}
	return out, nil
}

func decodePriceEstimation(raw []byte, req PriceEstimationRequest) (PriceEstimationResponse, error) {
	inner := unwrapData(raw)
	var wire struct {
		EstimatedCost   *float64        `json:"estimated_cost"`
		Currency        string          `json:"currency"`
		EstimationValid *bool           `json:"estimation_valid"`
		PriceBreakdown  *PriceBreakdown `json:"price_breakdown"`
		// provider 实际形状（参照侧注释里的 data 字段）
		TotalCostUSDC                *float64 `json:"total_cost_usdc"`
		IONetFee                     *float64 `json:"ionet_fee"`
		IONetFeePercent              *float64 `json:"ionet_fee_percent"`
		CurrencyConversionFee        *float64 `json:"currency_conversion_fee"`
		CurrencyConversionFeePercent *float64 `json:"currency_conversion_fee_percent"`
		Discount                     *float64 `json:"discount"`
	}
	if err := json.Unmarshal(inner, &wire); err != nil {
		return PriceEstimationResponse{}, &InvalidResponseError{Err: err}
	}
	out := PriceEstimationResponse{EstimationValid: true}
	switch {
	case wire.EstimatedCost != nil:
		out.EstimatedCost = *wire.EstimatedCost
	case wire.TotalCostUSDC != nil:
		out.EstimatedCost = *wire.TotalCostUSDC
	default:
		return PriceEstimationResponse{}, &InvalidResponseError{Err: errors.New("price response has no total cost")}
	}
	out.Currency = strings.ToUpper(strings.TrimSpace(wire.Currency))
	if out.Currency == "" {
		out.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	}
	if out.Currency == "" {
		out.Currency = "USDC"
	}
	if wire.EstimationValid != nil {
		out.EstimationValid = *wire.EstimationValid
	}
	if wire.PriceBreakdown != nil {
		out.PriceBreakdown = *wire.PriceBreakdown
		return out, nil
	}
	// 扁平 provider 形状：只填 provider 实际提供的费用项。
	out.PriceBreakdown = PriceBreakdown{
		IONetFee:                     wire.IONetFee,
		IONetFeePercent:              wire.IONetFeePercent,
		CurrencyConversionFee:        wire.CurrencyConversionFee,
		CurrencyConversionFeePercent: wire.CurrencyConversionFeePercent,
		Discount:                     wire.Discount,
		TotalCost:                    wire.TotalCostUSDC,
	}
	if wire.TotalCostUSDC != nil && wire.IONetFee != nil {
		compute := *wire.TotalCostUSDC - *wire.IONetFee
		if wire.CurrencyConversionFee != nil {
			compute -= *wire.CurrencyConversionFee
		}
		out.PriceBreakdown.ComputeCost = &compute
	}
	return out, nil
}

func decodeNameAvailability(raw []byte) (bool, error) {
	inner := unwrapData(raw)
	var available bool
	if err := json.Unmarshal(inner, &available); err == nil {
		return available, nil
	}
	var wrapped struct {
		Available *bool `json:"available"`
	}
	if err := json.Unmarshal(inner, &wrapped); err != nil {
		return false, &InvalidResponseError{Err: err}
	}
	if wrapped.Available == nil {
		return false, &InvalidResponseError{Err: errors.New("name availability response is missing 'available'")}
	}
	return *wrapped.Available, nil
}

func normalizeStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}
