// GET /api/instances（modules-08 §3.7 F-08-21）。
//
//	GET /api/instances   settings:manage
//
// 返回逐副本心跳列表与当前实例标识：
//
//	{items:[{instance_id,status,role,version,started_at,last_seen}],
//	 current_instance_id}
//
// 为什么不分页：实例数是部署规模（个位数到几十），分页只会把一次心跳扫描
// 拆成多轮并让前端多一层状态；与「列表必须分页」的业务表口径不同。
//
// 为什么权限是 settings:manage 而不是 admin-only：该端点给性能页的逐副本
// 操作页头提供当前实例标识（F-08-49），必须与性能端点的权限一致，避免
// 「能看到实例名但点不动操作」的半权限状态。
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/rbac"
)

const (
	instanceListProblemType = "https://newtg.dev/problems/instance_registry_unavailable"
	// 固定文案：Redis 错误详情可能包含地址/拓扑信息，绝不回显。
	instanceListUnavailableDetail = "instance registry is temporarily unavailable"
)

// instanceReadAction 是实例列表的 RBAC 动作。只读动作不登记字段白名单。
var instanceReadAction = rbac.AuthzAction{ResourceKind: "instance", ActionName: "read"}

// InstanceHandlerConfig 是实例列表端点的装配输入。
type InstanceHandlerConfig struct {
	Redis      *redis.Client
	InstanceID string
	Role       string
	Authz      *rbac.AuthzMiddleware
	Logger     *slog.Logger
}

// InstanceHandlers 持有实例列表依赖。
type InstanceHandlers struct {
	rdb        *redis.Client
	instanceID string
	role       string
	authz      *rbac.AuthzMiddleware
	log        *slog.Logger
}

// NewInstanceHandlers 构造端点。Authz 为 nil 时回退到 admin-only 门：
// 生产组合根必传 Authz；单测/最小装配不因缺少 RBAC 中间件而 panic。
func NewInstanceHandlers(cfg InstanceHandlerConfig) *InstanceHandlers {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	role := cfg.Role
	if role == "" {
		role = instanceRoleGateway
	}
	return &InstanceHandlers{
		rdb:        cfg.Redis,
		instanceID: cfg.InstanceID,
		role:       role,
		authz:      cfg.Authz,
		log:        log,
	}
}

// Register 挂载到受保护管理子树。
func (h *InstanceHandlers) Register(mux *http.ServeMux) {
	var gate func(http.Handler) http.Handler
	if h.authz != nil {
		gate = h.authz.Handler(rbac.PermSettingsManage, instanceReadAction)
	} else {
		gate = requireAdminRole
	}
	mux.Handle("GET /api/instances", gate(http.HandlerFunc(h.handleList)))
}

type instanceListResponse struct {
	Items             []InstanceRecord `json:"items"`
	CurrentInstanceID string           `json:"current_instance_id"`
}

func (h *InstanceHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	items, err := ListInstanceHeartbeats(r.Context(), h.rdb)
	if err != nil {
		// 只记录错误供运维排查；响应体固定，不泄露 Redis 地址或错误文本。
		h.log.Warn("list instances: redis read failed", "error", err)
		writeProblemDetail(w, http.StatusServiceUnavailable, instanceListProblemType,
			"Instance registry unavailable", instanceListUnavailableDetail)
		return
	}
	// 当前实例即使心跳写失败也必须在列表里：否则管理员在故障窗口内会看到
	// 「自己不存在」，页头实例标识随之消失。
	if h.instanceID != "" && !containsInstance(items, h.instanceID) {
		items = append(items, InstanceRecord{
			InstanceID: h.instanceID,
			Status:     instanceStatusLive,
			Role:       h.role,
		})
	}
	if items == nil {
		items = []InstanceRecord{}
	}
	writeJSON(w, http.StatusOK, instanceListResponse{
		Items:             items,
		CurrentInstanceID: h.instanceID,
	})
}

func containsInstance(items []InstanceRecord, id string) bool {
	for _, item := range items {
		if item.InstanceID == id {
			return true
		}
	}
	return false
}
