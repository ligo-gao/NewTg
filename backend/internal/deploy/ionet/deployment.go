// 部署控制面（io.net）的读方法与写方法。
//
// 设计权威：docs/modules-08-系统管理与设置-子功能设计.md §3.20.2 / §3.20.7。
//
// 分层约定（T4）：
//   - 本文件只负责「请求构造 → HTTP 一次发送（写）/有界重试（读）」；
//   - 所有 provider 响应形状的解析与字段映射集中在 adapter.go，因为 provider
//     真实响应字段名尚待实测（R-M6D-13），集中一处便于实测后单点修正；
//   - 写方法永不重试：provider 不保证幂等，重复提交可能重复计费。
package ionet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// PathDeployments 是部署集合端点（POST 创建 / GET 列表）。
	PathDeployments = apiBasePath + "/deployments"
	// PathPriceEstimation 是价格估算端点（GET + query）。
	PathPriceEstimation = apiBasePath + "/price"
	// PathAvailableReplicas 是各地域副本余量端点。
	PathAvailableReplicas = apiBasePath + "/available-replicas"
	// PathCheckName 是部署名称可用性探测端点。
	//
	// 参照侧用「集群名」语义（clusters/check_cluster_name_availability），
	// 部署名在 provider 侧就是 cluster name；实测确认前集中在此常量。
	PathCheckName = apiBasePath + "/clusters/check_cluster_name_availability"
)

// deploymentPath 返回 /deployment/{id}。ID 由客户端提供，必须转义后再拼进
// 路径，避免注入额外路径段。
func deploymentPath(id string) string {
	return apiBasePath + "/deployment/" + url.PathEscape(id)
}

// DeploymentListOptions 是列表查询参数。provider 不支持 keyword，网关联
// 全量扫描（见 handlers_deployments.go 的冻结上限）。
type DeploymentListOptions struct {
	Status    string
	Page      int
	PageSize  int
	SortBy    string
	SortOrder string
}

// DeploymentSummary 是列表一行（已归一化；字段形状由 adapter.go 负责）。
type DeploymentSummary struct {
	ID                      string
	Name                    string
	Status                  string
	CompletedPercent        float64
	GPUCount                int
	ReplicaCount            int
	HardwareName            string
	BrandName               string
	ComputeMinutesServed    int
	ComputeMinutesRemaining int
	CreatedAt               time.Time
}

// DeploymentPage 是一页部署列表。
type DeploymentPage struct {
	Items    []DeploymentSummary
	Total    int
	Statuses []string
}

// DeploymentLocation 是部署详情里的地域条目。
type DeploymentLocation struct {
	ID   int    `json:"id"`
	ISO2 string `json:"iso2"`
	Name string `json:"name"`
}

// DeploymentContainerConfig 是详情里的非秘密容器配置。
// 秘密环境变量不在 provider 详情响应中，也不得在本结构中出现。
type DeploymentContainerConfig struct {
	Entrypoint   []string
	EnvVariables map[string]any
	TrafficPort  int
	ImageURL     string
}

// DeploymentDetail 是部署概览（已归一化）。
type DeploymentDetail struct {
	ID                      string
	Status                  string
	CreatedAt               *time.Time
	StartedAt               *time.Time
	FinishedAt              *time.Time
	AmountPaid              float64
	CompletedPercent        float64
	TotalGPUs               int
	GPUsPerContainer        int
	TotalContainers         int
	HardwareID              int
	HardwareName            string
	BrandName               string
	Locations               []DeploymentLocation
	ComputeMinutesServed    int
	ComputeMinutesRemaining int
	ContainerConfig         DeploymentContainerConfig
}

// ContainerEvent 是容器事件。
type ContainerEvent struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// Container 是部署下的一个容器（已归一化）。
type Container struct {
	DeviceID         string
	ContainerID      string
	Hardware         string
	BrandName        string
	Status           string
	GPUsPerContainer int
	UptimePercent    int
	CreatedAt        *time.Time
	PublicURL        string
	Events           []ContainerEvent
}

// ContainerList 是部署容器列表。
type ContainerList struct {
	Containers []Container
	Total      int
}

// LogsOptions 是日志分页参数。PageToken 映射 provider 的 cursor；
// Follow 被刻意移除：本能力不提供长连接（§3.20.2）。
type LogsOptions struct {
	ContainerID string
	Level       string
	Stream      string
	StartTime   *time.Time
	EndTime     *time.Time
	PageToken   string
	Limit       int
}

// LogsPage 是日志分页结果。纯文本响应被拆成行数组（§3.20.2）。
type LogsPage struct {
	Lines         []string
	NextPageToken string
	HasMore       bool
}

// AvailableReplica 是某地域某硬件的可用副本数。
type AvailableReplica struct {
	LocationID     int    `json:"location_id"`
	LocationName   string `json:"location_name"`
	HardwareID     int    `json:"hardware_id"`
	AvailableCount int    `json:"available_count"`
	MaxGPUs        int    `json:"max_gpus"`
}

// AvailableReplicas 是副本余量响应。
type AvailableReplicas struct {
	Replicas []AvailableReplica `json:"replicas"`
}

// PriceEstimationRequest 是价格估算输入。Currency/DurationType/DurationQty/
// HardwareQty 为空时由 provider 默认值补齐（与参照侧一致）。
type PriceEstimationRequest struct {
	HardwareID       int
	LocationIDs      []int
	GPUsPerContainer int
	ReplicaCount     int
	DurationHours    int
	Currency         string
	DurationType     string
	DurationQty      int
	HardwareQty      int
}

// PriceBreakdown 是 provider 返回的费用分解。指针 + omitempty 让「缺失分解
// 只返回已有费用项」，不会用 0 伪造未提供的项。
type PriceBreakdown struct {
	ComputeCost                  *float64 `json:"compute_cost,omitempty"`
	NetworkCost                  *float64 `json:"network_cost,omitempty"`
	StorageCost                  *float64 `json:"storage_cost,omitempty"`
	TotalCost                    *float64 `json:"total_cost,omitempty"`
	HourlyRate                   *float64 `json:"hourly_rate,omitempty"`
	IONetFee                     *float64 `json:"ionet_fee,omitempty"`
	IONetFeePercent              *float64 `json:"ionet_fee_percent,omitempty"`
	CurrencyConversionFee        *float64 `json:"currency_conversion_fee,omitempty"`
	CurrencyConversionFeePercent *float64 `json:"currency_conversion_fee_percent,omitempty"`
	Discount                     *float64 `json:"discount,omitempty"`
}

// PriceEstimationResponse 是价格估算结果（金额/币种/分解原样映射）。
type PriceEstimationResponse struct {
	EstimatedCost   float64        `json:"estimated_cost"`
	Currency        string         `json:"currency"`
	PriceBreakdown  PriceBreakdown `json:"price_breakdown"`
	EstimationValid bool           `json:"estimation_valid"`
}

// DeploymentAction 是写操作的受理结果（创建/更新/续期/终止）。
type DeploymentAction struct {
	DeploymentID string
	Status       string
}

// CreateDeploymentRequest 是创建部署的归一化输入（秘密面单列）。
type CreateDeploymentRequest struct {
	Name               string
	HardwareID         int
	LocationIDs        []int
	GPUsPerContainer   int
	ReplicaCount       int
	DurationHours      int
	ImageURL           string
	RegistryUsername   string
	RegistrySecret     string
	Entrypoint         []string
	Args               []string
	TrafficPort        int
	EnvVariables       map[string]string
	SecretEnvVariables map[string]string
}

// UpdateDeploymentRequest 只表达「变更字段」：nil 指针 = 不更新。
// 秘密字段的显式清除用 Clear* 布尔位（provider 需要收到空值才会清除）。
type UpdateDeploymentRequest struct {
	ImageURL            *string
	RegistryUsername    *string
	RegistrySecret      *string
	ClearRegistrySecret bool
	Entrypoint          *[]string
	Args                *[]string
	TrafficPort         *int
	EnvVariables        *map[string]string
	SecretEnvVariables  *map[string]string
	ClearSecretEnv      bool
	Command             *string
}

// ListDeployments 读取一页部署列表（只读 GET，有界重试）。
func (c *Client) ListDeployments(ctx context.Context, opts DeploymentListOptions) (DeploymentPage, error) {
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Page > 0 {
		q.Set("page", strconv.Itoa(opts.Page))
	}
	if opts.PageSize > 0 {
		q.Set("page_size", strconv.Itoa(opts.PageSize))
	}
	if opts.SortBy != "" {
		q.Set("sort_by", opts.SortBy)
	}
	if opts.SortOrder != "" {
		q.Set("sort_order", opts.SortOrder)
	}
	path := PathDeployments
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	raw, err := c.getBytes(ctx, path)
	if err != nil {
		return DeploymentPage{}, err
	}
	return decodeDeploymentPage(raw)
}

// GetDeployment 读取部署概览。
func (c *Client) GetDeployment(ctx context.Context, id string) (DeploymentDetail, error) {
	raw, err := c.getBytes(ctx, deploymentPath(id))
	if err != nil {
		return DeploymentDetail{}, err
	}
	return decodeDeploymentDetail(raw, id)
}

// ListContainers 读取部署下的容器列表。
func (c *Client) ListContainers(ctx context.Context, id string) (ContainerList, error) {
	raw, err := c.getBytes(ctx, deploymentPath(id)+"/containers")
	if err != nil {
		return ContainerList{}, err
	}
	return decodeContainerList(raw)
}

// GetContainer 读取单个容器详情。
func (c *Client) GetContainer(ctx context.Context, id, containerID string) (Container, error) {
	raw, err := c.getBytes(ctx, deploymentPath(id)+"/container/"+url.PathEscape(containerID))
	if err != nil {
		return Container{}, err
	}
	return decodeContainer(raw)
}

// GetLogs 读取一页容器日志。provider 返回纯文本时按行拆分。
func (c *Client) GetLogs(ctx context.Context, id string, opts LogsOptions) (LogsPage, error) {
	q := url.Values{}
	if opts.Level != "" {
		q.Set("level", opts.Level)
	}
	if opts.Stream != "" {
		q.Set("stream", opts.Stream)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.PageToken != "" {
		q.Set("cursor", opts.PageToken)
	}
	if opts.StartTime != nil {
		q.Set("start_time", opts.StartTime.UTC().Format(time.RFC3339))
	}
	if opts.EndTime != nil {
		q.Set("end_time", opts.EndTime.UTC().Format(time.RFC3339))
	}
	path := publicAPIPath + "/deployment/" + url.PathEscape(id) + "/log/" + url.PathEscape(opts.ContainerID)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	raw, err := c.getBytes(ctx, path)
	if err != nil {
		return LogsPage{}, err
	}
	return decodeLogsPage(raw)
}

// AvailableReplicas 读取指定硬件的各地域余量。
func (c *Client) AvailableReplicas(ctx context.Context, hardwareID, gpuCount int) (AvailableReplicas, error) {
	q := url.Values{}
	q.Set("hardware_id", strconv.Itoa(hardwareID))
	q.Set("hardware_qty", strconv.Itoa(gpuCount))
	raw, err := c.getBytes(ctx, PathAvailableReplicas+"?"+q.Encode())
	if err != nil {
		return AvailableReplicas{}, err
	}
	return decodeAvailableReplicas(raw, hardwareID, gpuCount)
}

// EstimatePrice 读取 provider 报价（只读 GET）。
func (c *Client) EstimatePrice(ctx context.Context, req PriceEstimationRequest) (PriceEstimationResponse, error) {
	raw, err := c.getBytes(ctx, PathPriceEstimation+"?"+buildPriceQuery(req).Encode())
	if err != nil {
		return PriceEstimationResponse{}, err
	}
	return decodePriceEstimation(raw, req)
}

// CheckName 探测部署名可用性。不可用是正常结果（available=false），不是错误。
func (c *Client) CheckName(ctx context.Context, name string) (bool, error) {
	q := url.Values{}
	q.Set("cluster_name", name)
	raw, err := c.getBytes(ctx, PathCheckName+"?"+q.Encode())
	if err != nil {
		return false, err
	}
	return decodeNameAvailability(raw)
}

// CreateDeployment 创建部署（写：单次发送，不重试）。
func (c *Client) CreateDeployment(ctx context.Context, req CreateDeploymentRequest) (DeploymentAction, error) {
	body := buildCreateRequest(req)
	var out wireActionResponse
	if err := c.writeJSON(ctx, http.MethodPost, PathDeployments, body, &out,
		append([]string{req.RegistrySecret}, mapValues(req.SecretEnvVariables)...)...); err != nil {
		return DeploymentAction{}, err
	}
	return DeploymentAction{DeploymentID: out.DeploymentID, Status: out.Status}, nil
}

// UpdateDeployment 更新配置（写：单次发送，不重试）。
func (c *Client) UpdateDeployment(ctx context.Context, id string, req UpdateDeploymentRequest) (DeploymentAction, error) {
	body := buildUpdateRequest(req)
	secrets := []string{}
	if req.RegistrySecret != nil {
		secrets = append(secrets, *req.RegistrySecret)
	}
	if req.SecretEnvVariables != nil {
		secrets = append(secrets, mapValues(*req.SecretEnvVariables)...)
	}
	var out wireActionResponse
	if err := c.writeJSON(ctx, http.MethodPatch, deploymentPath(id), body, &out, secrets...); err != nil {
		return DeploymentAction{}, err
	}
	return DeploymentAction{DeploymentID: firstNonEmpty(out.DeploymentID, id), Status: out.Status}, nil
}

// RenameDeployment 改名（写：单次发送，不重试）。
func (c *Client) RenameDeployment(ctx context.Context, id, name string) (DeploymentAction, error) {
	body := map[string]string{"cluster_name": name}
	var out wireRenameResponse
	if err := c.writeJSON(ctx, http.MethodPut,
		apiBasePath+"/clusters/"+url.PathEscape(id)+"/update-name", body, &out); err != nil {
		return DeploymentAction{}, err
	}
	return DeploymentAction{DeploymentID: id, Status: out.Status}, nil
}

// ExtendDeployment 续期（写：单次发送，不重试）。
func (c *Client) ExtendDeployment(ctx context.Context, id string, hours int) (DeploymentAction, error) {
	body := map[string]int{"duration_hours": hours}
	// provider 续期接口返回 DeploymentDetail（无 deployment_id 字段时用路径 ID）。
	var out wireDeploymentDetail
	if err := c.writeJSON(ctx, http.MethodPost, deploymentPath(id)+"/extend", body, &out); err != nil {
		return DeploymentAction{}, err
	}
	return DeploymentAction{DeploymentID: firstNonEmpty(out.ID, id), Status: out.Status}, nil
}

// DeleteDeployment 终止部署（写：单次发送，不重试）。
func (c *Client) DeleteDeployment(ctx context.Context, id string) (DeploymentAction, error) {
	var out wireActionResponse
	if err := c.writeJSON(ctx, http.MethodDelete, deploymentPath(id), nil, &out); err != nil {
		return DeploymentAction{}, err
	}
	return DeploymentAction{DeploymentID: firstNonEmpty(out.DeploymentID, id), Status: out.Status}, nil
}

// mapValues 提取 map 的值（用于秘密值脱敏）。
func mapValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// buildCreateRequest 构造 provider 创建请求体（字段名参照侧实测形状）。
func buildCreateRequest(req CreateDeploymentRequest) map[string]any {
	container := map[string]any{
		"replica_count": req.ReplicaCount,
	}
	if len(req.EnvVariables) > 0 {
		container["env_variables"] = req.EnvVariables
	}
	if len(req.SecretEnvVariables) > 0 {
		container["secret_env_variables"] = req.SecretEnvVariables
	}
	if len(req.Entrypoint) > 0 {
		container["entrypoint"] = req.Entrypoint
	}
	if len(req.Args) > 0 {
		container["args"] = req.Args
	}
	if req.TrafficPort > 0 {
		container["traffic_port"] = req.TrafficPort
	}
	registry := map[string]any{"image_url": req.ImageURL}
	if req.RegistryUsername != "" {
		registry["registry_username"] = req.RegistryUsername
	}
	if req.RegistrySecret != "" {
		registry["registry_secret"] = req.RegistrySecret
	}
	return map[string]any{
		"resource_private_name": req.Name,
		"duration_hours":        req.DurationHours,
		"gpus_per_container":    req.GPUsPerContainer,
		"hardware_id":           req.HardwareID,
		"location_ids":          req.LocationIDs,
		"container_config":      container,
		"registry_config":       registry,
	}
}

// buildUpdateRequest 构造 provider 更新请求体。
//
// 用 map 而不是带 omitempty 的 struct：空 map / 空字符串在 provider 语义里
// 是「清除」，omitempty 会把它们静默丢掉（秘密清除就永远不生效）。
func buildUpdateRequest(req UpdateDeploymentRequest) map[string]any {
	body := map[string]any{}
	if req.ImageURL != nil {
		body["image_url"] = *req.ImageURL
	}
	if req.RegistryUsername != nil {
		body["registry_username"] = *req.RegistryUsername
	}
	switch {
	case req.ClearRegistrySecret:
		body["registry_secret"] = ""
	case req.RegistrySecret != nil:
		body["registry_secret"] = *req.RegistrySecret
	}
	if req.Entrypoint != nil {
		body["entrypoint"] = *req.Entrypoint
	}
	if req.Args != nil {
		body["args"] = *req.Args
	}
	if req.Command != nil {
		body["command"] = *req.Command
	}
	if req.TrafficPort != nil {
		body["traffic_port"] = *req.TrafficPort
	}
	if req.EnvVariables != nil {
		body["env_variables"] = *req.EnvVariables
	}
	switch {
	case req.ClearSecretEnv:
		body["secret_env_variables"] = map[string]string{}
	case req.SecretEnvVariables != nil:
		body["secret_env_variables"] = *req.SecretEnvVariables
	}
	return body
}

// buildPriceQuery 构造价格估算 query（默认值口径与参照侧一致）。
func buildPriceQuery(req PriceEstimationRequest) url.Values {
	currency := strings.TrimSpace(req.Currency)
	if currency == "" {
		currency = "usdc"
	}
	durationType := strings.ToLower(strings.TrimSpace(req.DurationType))
	durationQty := req.DurationQty
	if durationQty < 1 {
		durationQty = req.DurationHours
	}
	if durationQty < 1 {
		durationQty = 1
	}
	hardwareQty := req.HardwareQty
	if hardwareQty < 1 {
		hardwareQty = req.GPUsPerContainer
	}
	if hardwareQty < 1 {
		hardwareQty = 1
	}
	apiDurationType := "hourly"
	switch durationType {
	case "day", "days", "daily":
		apiDurationType = "daily"
	case "week", "weeks", "weekly":
		apiDurationType = "weekly"
	case "month", "months", "monthly":
		apiDurationType = "monthly"
	}

	locations, _ := json.Marshal(req.LocationIDs)
	q := url.Values{}
	q.Set("location_ids", string(locations))
	q.Set("hardware_id", strconv.Itoa(req.HardwareID))
	q.Set("hardware_qty", strconv.Itoa(hardwareQty))
	q.Set("gpus_per_container", strconv.Itoa(req.GPUsPerContainer))
	q.Set("duration_type", apiDurationType)
	q.Set("duration_qty", strconv.Itoa(durationQty))
	q.Set("duration_hours", strconv.Itoa(req.DurationHours))
	q.Set("replica_count", strconv.Itoa(req.ReplicaCount))
	q.Set("currency", currency)
	return q
}
