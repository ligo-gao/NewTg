package adapters

import (
	"errors"
	"fmt"

	"newtg/gateway/internal/contract"
)

// ============================================================================
// 适配器错误与契约错误分类的桥接（契约 §4）
//
// 契约把错误语义冻结为 `contract.ErrorClass` 枚举值；枚举本身不是 Go error。
// 适配器接口返回 `error`，执行层又要据此决定「是否重试 / 是否自动禁用」，
// 因此这里提供一个携带 ErrorClass 的 error 包装：
//   - `errors.Is(err, ErrEndpointUnsupported)` 可判定具体哨兵；
//   - `ClassOf(err)` 可取得契约分类（未知分类返回 false）。
// ============================================================================

// Error 是带契约分类的适配器错误。
type Error struct {
	// Class 是契约 §4 的错误分类；执行层据三态判定顺序决定重试（relay-01 §6.3）。
	Class contract.ErrorClass
	Err   error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Is 让同一契约分类的所有错误都能命中分类哨兵：
// `errors.Is(err, ErrChannelConfig)` 对任何 ErrChannelConfig 类错误成立，
// 而无需每个构造点都显式包装哨兵。target 不是 *Error 时按默认指针比较。
func (e *Error) Is(target error) bool {
	var other *Error
	if errors.As(target, &other) && other != nil {
		return e.Class == other.Class
	}
	return false
}

// ClassOf 提取错误链中最近一层契约分类。
func ClassOf(err error) (contract.ErrorClass, bool) {
	var classified *Error
	if errors.As(err, &classified) && classified != nil {
		return classified.Class, true
	}
	return contract.ErrUnknown, false
}

func newSentinel(class contract.ErrorClass, message string) *Error {
	return &Error{Class: class, Err: errors.New(message)}
}

// classifiedf 构造带指定分类的错误；message 不得包含 base_url / 凭据（GC-6）。
func classifiedf(class contract.ErrorClass, format string, args ...any) error {
	return &Error{Class: class, Err: fmt.Errorf(format, args...)}
}

// 适配器层对外暴露的分类哨兵。
var (
	// ErrEndpointUnsupported：该渠道不支持该端点/无适配器实现（不重试、不自动禁用，
	// 契约 §4 · 决议 E2）。21/61 的显式失败与未实现键都使用它（relay-02 §2.3）。
	ErrEndpointUnsupported = newSentinel(contract.ErrEndpointUnsupported, "endpoint unsupported")
	// ErrParamInvalid：请求参数非法（不重试，契约 §4.1）。
	ErrParamInvalid = newSentinel(contract.ErrParamInvalid, "invalid parameter")
	// ErrChannelConfig：渠道配置错误（不重试、可自动禁用，契约 §4.1）。
	ErrChannelConfig = newSentinel(contract.ErrChannelConfig, "invalid channel configuration")
	// ErrUpstreamAuth：上游鉴权失败（凭据问题，契约 §4.1）。
	ErrUpstreamAuth = newSentinel(contract.ErrUpstreamAuth, "upstream authentication failed")
	// ErrUpstreamRateLimit：上游限流（契约 §4.1）。
	ErrUpstreamRateLimit = newSentinel(contract.ErrUpstreamRateLimit, "upstream rate limited")
	// ErrUpstreamServer：上游 5xx（契约 §4.1）。
	ErrUpstreamServer = newSentinel(contract.ErrUpstreamServer, "upstream server error")
	// ErrRetryable：上游瞬时故障（重试区间，契约 §4.1）。
	ErrRetryable = newSentinel(contract.ErrRetryable, "retryable upstream error")
)
