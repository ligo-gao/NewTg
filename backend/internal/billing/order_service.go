package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"newtg/gateway/internal/store"
)

// 本文件是订单中心与人工旁路的服务层（modules-04 §3/§4.D/§4.E；
// 裁决 P-1/P-9/P-18/P-19）。
//
// 边界：
//   - 归属/权限判定属于 HTTP 层（P-18：他人订单表现为 404）；本层只接收
//     已鉴权 handler 找到的订单，负责状态机、渠道查询与入账。
//   - 所有状态迁移仍然只走条件 UPDATE（WHERE status='pending'）。这里的
//     应用层 Transition 检查只是把冲突提前成可读错误，不能替代 DB 条件。
//   - 入账只复用 settleOrder：webhook、主动核实、人工完成必须是同一条路径，
//     否则三处的幂等语义会分叉。

const (
	// OrderExpiry 是订单有效期（modules-04 F-04-07 / P-1：30 分钟）。
	// 冻结为派生值，不新增 expires_at 列——到期时间由 created_at 计算，
	// 避免 worker 与读路径对「过期」出现两个真源。
	OrderExpiry = 30 * time.Minute

	// manualReasonMinRunes / manualReasonMaxRunes 与 orders_manual_reason_chk
	// 的 char_length BETWEEN 10 AND 200 逐字一致。按 rune 计数以匹配
	// PostgreSQL char_length（中文按字符而不是字节）。
	manualReasonMinRunes = 10
	manualReasonMaxRunes = 200
)

var (
	// ErrOrderNotPaid 表示渠道明确回答「未收款」。对 verify 是 200 的
	// 业务结果，对 complete 是 400（人工完成必须先核实收款）。
	ErrOrderNotPaid = errors.New("billing: order is not paid at provider")
	// ErrInvalidManualReason 表示人工完成原因不满足 10–200 字符。
	ErrInvalidManualReason = errors.New("billing: invalid manual completion reason")
)

// OrderListFilter 是订单列表的查询条件。UserID=0 表示管理员全局视图
// （权限已在 handler 判定）；Query 是订单号子串。
type OrderListFilter struct {
	UserID   int64
	Status   string
	Query    string
	Page     int
	PageSize int
}

// ListOrders 返回倒序（created_at DESC, id DESC）的一页订单与总数。
func (s *TopupService) ListOrders(ctx context.Context, filter OrderListFilter) ([]store.Order, int64, error) {
	page, pageSize := filter.Page, filter.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	buildQuery := func() *gorm.DB {
		query := s.db.WithContext(ctx).Model(&store.Order{})
		if filter.UserID > 0 {
			query = query.Where("user_id = ?", filter.UserID)
		}
		if filter.Status != "" {
			query = query.Where("status = ?", filter.Status)
		}
		if filter.Query != "" {
			// 订单号子串搜索。ESC 显式声明，避免用户输入的 %/_ 被当成通配符
			// 把「搜索」放大成全表扫描。
			query = query.Where(`CAST(id AS TEXT) LIKE ? ESCAPE '\'`,
				"%"+escapeLikePattern(filter.Query)+"%")
		}
		return query
	}
	var total int64
	if err := buildQuery().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("billing: count orders: %w", err)
	}
	orders := make([]store.Order, 0, pageSize)
	if err := buildQuery().Order("created_at DESC").Order("id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&orders).Error; err != nil {
		return nil, 0, fmt.Errorf("billing: list orders: %w", err)
	}
	return orders, total, nil
}

// escapeLikePattern 转义 LIKE 元字符（\\、%、_）。
func escapeLikePattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// GetOrder 按主键读取订单。
func (s *TopupService) GetOrder(ctx context.Context, id int64) (*store.Order, error) {
	var order store.Order
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: order %d", ErrOrderNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("billing: get order %d: %w", id, err)
	}
	return &order, nil
}

// CancelOrder 把 pending 订单迁移到 cancelled，然后尽力关闭渠道支付单。
//
// 顺序刻意是「先 DB、后渠道」：渠道关单失败不能把订单留在 pending，
// 否则用户已取消却仍可能被扫码支付；而渠道关单成功但 DB 失败会让订单
// 变成「可支付但显示取消」——两种不一致中前者更危险。
func (s *TopupService) CancelOrder(ctx context.Context, order *store.Order) error {
	if err := Transition(OrderStatus(order.Status), OrderCancelled); err != nil {
		return fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
	}
	res := s.db.WithContext(ctx).Exec(
		`UPDATE orders SET status = 'cancelled', updated_at = now()
		 WHERE id = ? AND status = 'pending'`, order.ID)
	if res.Error != nil {
		return fmt.Errorf("billing: cancel order %d: %w", order.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		// 并发回调/取消已迁移：终态不可改写。
		return fmt.Errorf("%w: order %d changed concurrently", ErrOrderStateConflict, order.ID)
	}
	// DB 已经写入 cancelled/updated_at；重新读取后再返回，避免 handler
	// 序列化插入时的内存快照（created_at 与旧 updated_at）。
	if err := s.refreshOrder(ctx, order); err != nil {
		return err
	}
	s.closeProviderOrder(ctx, order)
	return nil
}

// refreshOrder 用数据库行覆盖内存中的订单。
//
// 为什么入账/取消后必须重读：状态迁移由 SQL 完成（status/paid_at/
// updated_at/credited_credits/raw_callback 等同事务写入），内存结构只有
// 服务层显式赋值的少数字段。响应直接序列化内存值会返回「succeeded 但
// paid_at=null」或并发落败路径的「paid=true 但 status=pending」。
func (s *TopupService) refreshOrder(ctx context.Context, order *store.Order) error {
	refreshed, err := s.GetOrder(ctx, order.ID)
	if err != nil {
		return fmt.Errorf("billing: refresh order %d: %w", order.ID, err)
	}
	*order = *refreshed
	return nil
}

// closeProviderOrder 是取消/超时回收后的 best-effort 渠道关单。失败只
// 告警：用户的取消意图或 worker 的过期判定已经落库，渠道侧残留由下一轮
// 回收/渠道自身过期兜底。
func (s *TopupService) closeProviderOrder(ctx context.Context, order *store.Order) {
	if order.ProviderTxnID == nil || strings.TrimSpace(*order.ProviderTxnID) == "" {
		return
	}
	var err error
	switch order.Provider {
	case ProviderStripe:
		err = s.CancelStripePaymentIntent(ctx, *order.ProviderTxnID)
	case ProviderAlipay:
		err = s.CloseAlipayTrade(ctx, *order.ProviderTxnID)
	default:
		return
	}
	if err != nil {
		s.log.Warn("close provider order failed",
			"order_id", order.ID, "provider", order.Provider, "error", err)
	}
}

// OrderVerification 是主动核实的结果。Paid=false 是业务结果（200），
// 不是错误：渠道正常回答「还没收到钱」。
type OrderVerification struct {
	Paid  bool
	Order *store.Order
}

// providerPaymentState 是渠道查询结果的渠道无关投影。
type providerPaymentState struct {
	TxnID    string
	Status   string
	Amount   int64
	Currency string
	Paid     bool
}

// VerifyOrder 主动查询渠道并在已支付时复用 webhook 的入账路径。
//
// 只有 pending 可核实：终态订单的迟到支付必须人工处理（金额核对后
// 通过 complete 或退款），绝不能因为一次查询把 expired/cancelled 改回
// 入账——那会绕过状态机的唯一合法迁移。
func (s *TopupService) VerifyOrder(ctx context.Context, order *store.Order) (*OrderVerification, error) {
	if err := Transition(OrderStatus(order.Status), OrderSucceeded); err != nil {
		return nil, fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
	}
	state, err := s.queryProviderPayment(ctx, order)
	if err != nil {
		return nil, err
	}
	if !state.Paid {
		return &OrderVerification{Paid: false, Order: order}, nil
	}
	raw := marshalWhitelisted(map[string]any{
		"provider":        order.Provider,
		"source":          "verify",
		"provider_txn_id": state.TxnID,
		"status":          state.Status,
		"amount":          state.Amount,
		"currency":        state.Currency,
	})
	if err := s.settleOrder(ctx, order, state.Amount, state.Currency, raw); err != nil {
		return nil, err
	}
	return &OrderVerification{Paid: true, Order: order}, nil
}

// CompleteOrder 是管理员人工完成旁路（F-04-12 / P-19）。
//
// 硬约束：渠道必须先确认收款，且金额/币种与订单一致；查询失败或未收款
// 一律 400 且不写任何状态。manual_* 三列与入账仍在同一订单行上，但
// manual 标记先落库、入账失败时回滚标记，避免出现「标记了人工完成但没到账」。
func (s *TopupService) CompleteOrder(ctx context.Context, order *store.Order,
	operatorID int64, reason string) error {

	if operatorID <= 0 {
		return fmt.Errorf("%w: operator id must be > 0", ErrInvalidManualReason)
	}
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < manualReasonMinRunes ||
		n > manualReasonMaxRunes || hasControlChars(reason) {
		return ErrInvalidManualReason
	}
	if err := Transition(OrderStatus(order.Status), OrderSucceeded); err != nil {
		return fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
	}
	state, err := s.queryProviderPayment(ctx, order)
	if err != nil {
		return err
	}
	if !state.Paid {
		return fmt.Errorf("%w: order %d", ErrOrderNotPaid, order.ID)
	}
	// 金额一致性在写 manual 标记前判定：金额不符的订单应保持 pending 等
	// 人工核对，而不是留下「已人工完成」的错误痕迹。
	if state.Currency != "" && state.Currency != order.Currency {
		return fmt.Errorf("%w: order %d", ErrCurrencyMismatch, order.ID)
	}
	if state.Amount != order.Amount {
		return fmt.Errorf("%w: order %d expected %d got %d",
			ErrAmountMismatch, order.ID, order.Amount, state.Amount)
	}

	res := s.db.WithContext(ctx).Exec(`
		UPDATE orders
		SET manual_completed = true, manual_reason = ?, manual_operator_id = ?, updated_at = now()
		WHERE id = ? AND status = 'pending'`, reason, operatorID, order.ID)
	if res.Error != nil {
		return fmt.Errorf("billing: mark order %d manual: %w", order.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: order %d changed concurrently", ErrOrderStateConflict, order.ID)
	}

	raw := marshalWhitelisted(map[string]any{
		"provider":           order.Provider,
		"source":             "manual_complete",
		"provider_txn_id":    state.TxnID,
		"status":             state.Status,
		"amount":             state.Amount,
		"currency":           state.Currency,
		"manual_operator_id": operatorID,
		"manual_reason":      reason,
	})
	if err := s.settleOrder(ctx, order, state.Amount, state.Currency, raw); err != nil {
		// 入账失败（含并发取消/金额竞态）：清掉 manual 标记，保持「未完成」
		// 的可重试语义。succeeded 行不会经过这里。
		s.clearManualCompletion(ctx, order.ID)
		return err
	}
	order.ManualCompleted = true
	order.ManualReason = &reason
	order.ManualOperatorID = &operatorID
	return nil
}

func (s *TopupService) clearManualCompletion(ctx context.Context, orderID int64) {
	if err := s.db.WithContext(ctx).Exec(`
		UPDATE orders
		SET manual_completed = false, manual_reason = NULL, manual_operator_id = NULL, updated_at = now()
		WHERE id = ? AND status <> 'succeeded'`, orderID).Error; err != nil {
		s.log.Error("clear manual completion failed", "order_id", orderID, "error", err)
	}
}

// queryProviderPayment 按渠道主动查询支付状态。
//
// 所有失败都收敛为 ErrProviderUnavailable（HTTP 层据此选择 502/400），
// 响应体不携带渠道原始错误，避免把密钥或渠道内部信息带出边界。
// 例外：渠道根本不支持查询（epay）返回 ErrProviderActionUnsupported，
// 由 HTTP 层映射为 409/400——那是「动作不可用」而非「渠道故障」。
func (s *TopupService) queryProviderPayment(ctx context.Context, order *store.Order) (*providerPaymentState, error) {
	// epay 协议没有主动查询接口（F-04-21 明确声明）：这是渠道能力缺失，
	// 不是瞬时故障。若落到 default 分支返回 ErrProviderDisabled，HTTP 层
	// 会把它解释成 502「渠道不可用」，误导用户重试。这里先于 txn 检查返回
	// ErrProviderActionUnsupported，由 HTTP 层回 409/400 固定文案。
	if order.Provider == ProviderEpay {
		return nil, fmt.Errorf("%w: %s has no active query API", ErrProviderActionUnsupported, ProviderEpay)
	}
	if order.ProviderTxnID == nil || strings.TrimSpace(*order.ProviderTxnID) == "" {
		return nil, fmt.Errorf("%w: order %d has no provider transaction id",
			ErrProviderUnavailable, order.ID)
	}
	txn := *order.ProviderTxnID
	switch order.Provider {
	case ProviderStripe:
		intent, err := s.QueryStripePaymentIntent(ctx, txn)
		if err != nil {
			return nil, fmt.Errorf("%w: stripe query", ErrProviderUnavailable)
		}
		amount := intent.AmountReceived
		if amount == 0 {
			amount = intent.Amount
		}
		return &providerPaymentState{
			TxnID:    intent.ID,
			Status:   intent.Status,
			Amount:   amount,
			Currency: strings.ToUpper(intent.Currency),
			Paid:     intent.Status == "succeeded",
		}, nil
	case ProviderAlipay:
		trade, err := s.QueryAlipayTrade(ctx, txn)
		if err != nil {
			return nil, fmt.Errorf("%w: alipay query", ErrProviderUnavailable)
		}
		paid := trade.TradeStatus == "TRADE_SUCCESS" || trade.TradeStatus == "TRADE_FINISHED"
		var amount int64
		if paid {
			// 未支付时 total_amount 可能为空；只有确认收款才需要金额比对。
			parsed, parseErr := yuanToCents(trade.TotalAmount)
			if parseErr != nil {
				return nil, fmt.Errorf("%w: alipay total_amount", ErrProviderUnavailable)
			}
			amount = parsed
		}
		return &providerPaymentState{
			TxnID:    trade.OutTradeNo,
			Status:   trade.TradeStatus,
			Amount:   amount,
			Currency: order.Currency, // 支付宝查询不回传币种；订单只有 CNY（M5 无汇率）
			Paid:     paid,
		}, nil
	default:
		return nil, fmt.Errorf("%w: provider %s", ErrProviderDisabled, order.Provider)
	}
}

// StripePaymentIntentState 是 `GET /v1/payment_intents/{id}` 的对账字段。
type StripePaymentIntentState struct {
	ID             string
	Amount         int64
	AmountReceived int64
	Currency       string
	Status         string
}

// QueryStripePaymentIntent 主动查询 PaymentIntent（F-04-12 回调丢失兜底）。
func (s *TopupService) QueryStripePaymentIntent(ctx context.Context, id string) (*StripePaymentIntentState, error) {
	secret, err := s.configSecret(ConfigStripeSecretKey)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.stripeBaseURL+"/v1/payment_intents/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("billing: stripe retrieve request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("billing: read stripe response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &providerHTTPError{provider: "stripe", status: resp.StatusCode}
	}
	var parsed struct {
		ID             string `json:"id"`
		Amount         int64  `json:"amount"`
		AmountReceived int64  `json:"amount_received"`
		Currency       string `json:"currency"`
		Status         string `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID == "" {
		return nil, fmt.Errorf("billing: parse stripe payment intent")
	}
	return &StripePaymentIntentState{
		ID:             parsed.ID,
		Amount:         parsed.Amount,
		AmountReceived: parsed.AmountReceived,
		Currency:       parsed.Currency,
		Status:         parsed.Status,
	}, nil
}

// CancelStripePaymentIntent 关闭未支付的 PaymentIntent（用户取消/超时）。
func (s *TopupService) CancelStripePaymentIntent(ctx context.Context, id string) error {
	secret, err := s.configSecret(ConfigStripeSecretKey)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.stripeBaseURL+"/v1/payment_intents/"+url.PathEscape(id)+"/cancel", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("billing: stripe cancel request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 丢弃响应体（限流读取）即可：调用方只关心是否成功。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &providerHTTPError{provider: "stripe", status: resp.StatusCode}
	}
	return nil
}

// OrderExpiresAt 返回订单的派生过期时间（created_at + 30min）。
func OrderExpiresAt(createdAt time.Time) time.Time {
	return createdAt.Add(OrderExpiry)
}
