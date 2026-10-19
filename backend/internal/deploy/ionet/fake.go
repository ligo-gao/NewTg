package ionet

import (
	"context"
	"sync"
)

// FakeProvider 是 DeploymentProvider 的测试替身（handler 单测与集成测试共用）。
//
// 放在非 _test 文件里是刻意的：httpapi 包的测试需要跨包使用它，而 Go 的
// _test.go 文件不会导出给其它包。它不包含任何生产逻辑，仅记录调用次数，
// 便于断言「路由是否真的探测了 provider」。
type FakeProvider struct {
	mu sync.Mutex

	Connection    ConnectionInfo
	ConnectionErr error

	Hardware    HardwareTypesResponse
	HardwareErr error

	Locations    LocationsResponse
	LocationsErr error

	ConnectionCalls int
	HardwareCalls   int
	LocationCalls   int
}

var _ DeploymentProvider = (*FakeProvider)(nil)

// TestConnection 实现 DeploymentProvider。
func (f *FakeProvider) TestConnection(ctx context.Context) (ConnectionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ConnectionCalls++
	return f.Connection, f.ConnectionErr
}

// ListHardware 实现 DeploymentProvider。
func (f *FakeProvider) ListHardware(ctx context.Context) (HardwareTypesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.HardwareCalls++
	return f.Hardware, f.HardwareErr
}

// ListLocations 实现 DeploymentProvider。
func (f *FakeProvider) ListLocations(ctx context.Context) (LocationsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LocationCalls++
	return f.Locations, f.LocationsErr
}

// Calls 返回三个方法的调用计数（快照）。
func (f *FakeProvider) Calls() (connection, hardware, locations int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ConnectionCalls, f.HardwareCalls, f.LocationCalls
}
