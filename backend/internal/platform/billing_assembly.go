package platform

import (
	"log/slog"

	"gorm.io/gorm"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/contract"
)

// initBillingAssembly 完成计费包的进程级装配，gateway 与 worker 共用。
//
// 两件副作用都必须在 configcenter.Load 之后执行（配置快照先就绪）：
//
//   - pricing.NewSource 读取 billing.quota_per_unit 并写入进程级
//     SetQuotaPerUnit。admin 定价 API 的 list/preview 换算
//     （quota_per_unit 字段、元↔积分）读的是 CurrentQuotaPerUnit，
//     因此 **gateway 也必须构造定价源**——只装配 worker 会让管理面
//     永远按默认 100 换算（复审 N2/R-83）。
//   - pricing.SetClampLogger 接 slog：billing-07 §4.5 要求每个
//     QuotaClamp 都留下 SysError 级后端日志。钩子是进程级的，两个角色
//     都要接线（复审 N5）。
//
// 返回 source 供计费引擎使用；gateway 当前只需要构造副作用，丢弃即可。
// QPU 非法（非整数/越界/非数字）返回 error，调用方必须让启动失败，
// 绝不能带着错误汇率对外服务。
func initBillingAssembly(gormDB *gorm.DB, center pricing.ConfigReader,
	cfg *config.Config, log *slog.Logger, role string) (*pricing.DBSource, error) {

	source, err := pricing.NewSource(gormDB, center)
	if err != nil {
		return nil, err
	}
	configureClampLogger(log, cfg, role)
	return source, nil
}

// configureClampLogger 把配额钳制审计接到结构化日志。
//
// 级别必须是 Error（billing-07 的 SysError）：钳制意味着上游用量或配置
// 产生了越界金额，结算已按饱和值继续，必须能被告警与检索。固定字段
// role/node 让多副本部署能定位到具体进程。
//
// clamp 复用 pricing.ClampAuditMap：original 为 NaN/±Inf 时已转成 JSON
// 安全表示，日志编码不会失败；键集合固定为 op/kind/original/clamped。
func configureClampLogger(log *slog.Logger, cfg *config.Config, role string) {
	if log == nil {
		log = slog.Default()
	}
	node := ""
	if cfg != nil {
		node = cfg.NodeName
	}
	pricing.SetClampLogger(func(c *contract.QuotaClamp) {
		if c == nil {
			return
		}
		log.Error("billing quota clamp",
			"role", role,
			"node", node,
			"clamp", pricing.ClampAuditMap(c),
		)
	})
}
