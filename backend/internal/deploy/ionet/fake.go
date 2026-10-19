package ionet

import (
	"context"
	"sync"
)

// FakeProvider 是 DeploymentProvider 的测试替身（handler 单测与集成测试共用）。
//
// 放在非 _test 文件里是刻意的：httpapi 包的测试需要跨包使用它，而 Go 的
// _test.go 文件不会导出给其它包。它不包含任何生产逻辑，仅记录调用次数与
// 最后一次入参，便于断言「路由是否真的探测了 provider」「写方法是否被重试」。
type FakeProvider struct {
	mu sync.Mutex

	Connection    ConnectionInfo
	ConnectionErr error

	Hardware    HardwareTypesResponse
	HardwareErr error

	Locations    LocationsResponse
	LocationsErr error

	Deployments      []DeploymentSummary
	DeploymentsTotal int // >0 时覆盖 len(Deployments)，用于模拟跨页总量
	// OmitDeploymentsTotal 模拟 provider 不返回 total 的真实兼容场景：
	// 扫描必须靠「本页不足一页」而不是伪造的总量来判断是否取全。
	OmitDeploymentsTotal bool
	DeploymentsErr       error

	DeploymentDetail    DeploymentDetail
	DeploymentDetailErr error

	Containers    ContainerList
	ContainersErr error

	Container    Container
	ContainerErr error

	Logs    LogsPage
	LogsErr error

	Replicas    AvailableReplicas
	ReplicasErr error

	Price    PriceEstimationResponse
	PriceErr error

	NameAvailable bool
	NameErr       error

	CreateResult DeploymentAction
	CreateErr    error

	UpdateResult DeploymentAction
	UpdateErr    error

	RenameResult DeploymentAction
	RenameErr    error

	ExtendResult DeploymentAction
	ExtendErr    error

	DeleteResult DeploymentAction
	DeleteErr    error

	// 调用计数与最后一次入参（写方法用于断言「不自动重试」与请求形状）。
	ConnectionCalls  int
	HardwareCalls    int
	LocationCalls    int
	DeploymentsCalls int
	DetailCalls      int
	ContainersCalls  int
	ContainerCalls   int
	LogsCalls        int
	ReplicasCalls    int
	PriceCalls       int
	NameCalls        int
	CreateCalls      int
	UpdateCalls      int
	RenameCalls      int
	ExtendCalls      int
	DeleteCalls      int

	LastListOptions     DeploymentListOptions
	LastLogsOptions     LogsOptions
	LastPriceRequest    PriceEstimationRequest
	LastCreateRequest   CreateDeploymentRequest
	LastUpdateRequest   UpdateDeploymentRequest
	LastUpdateID        string
	LastRenameName      string
	LastRenameID        string
	LastExtendHours     int
	LastExtendID        string
	LastDeleteID        string
	LastAvailableID     int
	LastAvailableGPU    int
	LastContainerLookup [2]string
}

var _ DeploymentProvider = (*FakeProvider)(nil)

// TestConnection 实现 DeploymentProvider。
func (f *FakeProvider) TestConnection(context.Context) (ConnectionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ConnectionCalls++
	return f.Connection, f.ConnectionErr
}

// ListHardware 实现 DeploymentProvider。
func (f *FakeProvider) ListHardware(context.Context) (HardwareTypesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.HardwareCalls++
	return f.Hardware, f.HardwareErr
}

// ListLocations 实现 DeploymentProvider。
func (f *FakeProvider) ListLocations(context.Context) (LocationsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LocationCalls++
	return f.Locations, f.LocationsErr
}

// ListDeployments 按 Page/PageSize 切片返回，模拟 provider 分页。
func (f *FakeProvider) ListDeployments(_ context.Context, opts DeploymentListOptions) (DeploymentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeploymentsCalls++
	f.LastListOptions = opts
	if f.DeploymentsErr != nil {
		return DeploymentPage{}, f.DeploymentsErr
	}
	total := f.DeploymentsTotal
	if total == 0 && !f.OmitDeploymentsTotal {
		total = len(f.Deployments)
	}
	if f.OmitDeploymentsTotal {
		total = 0
	}
	page := opts.Page
	if page < 1 {
		page = 1
	}
	size := opts.PageSize
	if size <= 0 {
		size = len(f.Deployments)
	}
	start := (page - 1) * size
	if start > len(f.Deployments) {
		start = len(f.Deployments)
	}
	end := start + size
	if end > len(f.Deployments) {
		end = len(f.Deployments)
	}
	items := append([]DeploymentSummary(nil), f.Deployments[start:end]...)
	return DeploymentPage{Items: items, Total: total}, nil
}

// GetDeployment 实现 DeploymentProvider。
func (f *FakeProvider) GetDeployment(_ context.Context, id string) (DeploymentDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DetailCalls++
	if f.DeploymentDetailErr != nil {
		return DeploymentDetail{}, f.DeploymentDetailErr
	}
	detail := f.DeploymentDetail
	if detail.ID == "" {
		detail.ID = id
	}
	return detail, nil
}

// ListContainers 实现 DeploymentProvider。
func (f *FakeProvider) ListContainers(context.Context, string) (ContainerList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ContainersCalls++
	return f.Containers, f.ContainersErr
}

// GetContainer 实现 DeploymentProvider。
func (f *FakeProvider) GetContainer(_ context.Context, id, containerID string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ContainerCalls++
	f.LastContainerLookup = [2]string{id, containerID}
	return f.Container, f.ContainerErr
}

// GetLogs 实现 DeploymentProvider。
func (f *FakeProvider) GetLogs(_ context.Context, _ string, opts LogsOptions) (LogsPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LogsCalls++
	f.LastLogsOptions = opts
	return f.Logs, f.LogsErr
}

// AvailableReplicas 实现 DeploymentProvider。
func (f *FakeProvider) AvailableReplicas(_ context.Context, hardwareID, gpuCount int) (AvailableReplicas, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReplicasCalls++
	f.LastAvailableID = hardwareID
	f.LastAvailableGPU = gpuCount
	return f.Replicas, f.ReplicasErr
}

// EstimatePrice 实现 DeploymentProvider。
func (f *FakeProvider) EstimatePrice(_ context.Context, req PriceEstimationRequest) (PriceEstimationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PriceCalls++
	f.LastPriceRequest = req
	return f.Price, f.PriceErr
}

// CheckName 实现 DeploymentProvider。
func (f *FakeProvider) CheckName(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.NameCalls++
	return f.NameAvailable, f.NameErr
}

// CreateDeployment 实现 DeploymentProvider。
func (f *FakeProvider) CreateDeployment(_ context.Context, req CreateDeploymentRequest) (DeploymentAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreateCalls++
	f.LastCreateRequest = req
	return f.CreateResult, f.CreateErr
}

// UpdateDeployment 实现 DeploymentProvider。
func (f *FakeProvider) UpdateDeployment(_ context.Context, id string, req UpdateDeploymentRequest) (DeploymentAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.UpdateCalls++
	f.LastUpdateID = id
	f.LastUpdateRequest = req
	return f.UpdateResult, f.UpdateErr
}

// RenameDeployment 实现 DeploymentProvider。
func (f *FakeProvider) RenameDeployment(_ context.Context, id, name string) (DeploymentAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RenameCalls++
	f.LastRenameID = id
	f.LastRenameName = name
	return f.RenameResult, f.RenameErr
}

// ExtendDeployment 实现 DeploymentProvider。
func (f *FakeProvider) ExtendDeployment(_ context.Context, id string, hours int) (DeploymentAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ExtendCalls++
	f.LastExtendID = id
	f.LastExtendHours = hours
	return f.ExtendResult, f.ExtendErr
}

// Calls 返回三个基础只读方法的调用计数（快照）。
func (f *FakeProvider) Calls() (connection, hardware, locations int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ConnectionCalls, f.HardwareCalls, f.LocationCalls
}

// DeleteDeployment 实现 DeploymentProvider。
func (f *FakeProvider) DeleteDeployment(_ context.Context, id string) (DeploymentAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeleteCalls++
	f.LastDeleteID = id
	return f.DeleteResult, f.DeleteErr
}
