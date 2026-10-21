package platform

// S4：把 billing 的自动续费失败通知接到站内通知投递表（P-16）。
//
// 【为什么在 platform 做适配】billing 不应该依赖 notify（计费核心不能反向
// 绑定通知实现）；worker 只依赖 RenewalFailureNotifier 这个窄接口。这里用
// 同一张 notification_deliveries 表与同一套 InAppPayload 形状，用户侧读取
// 端点（/api/notifications）无需任何改动。
//
// 【为什么用 billing.payment_failed 分类】注册表是 16 个冻结 category 的
// 封闭集合（§3.9.1），新增 category 需要同步 7 语模板与前端文案。自动续费
// 失败在语义上就是「支付失败」，复用既有分类让用户侧图标/筛选直接可用；
// 具体原因（card_declined 等）放在 payload 正文里。
//
// 【为什么按用户语言渲染】站内通知直接展示 payload，不经过模板回退链；
// 用 users.language 选择文案，缺省 en（与 DefaultLocale 一致）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/notify"
)

// renewalFailureNotifier 是 billing.RenewalFailureNotifier 的生产实现。
type renewalFailureNotifier struct {
	db    *sql.DB
	store *notify.Store
	// email 为 nil 时只做站内直投（W10-EM1 的 email 生产者按装配注入）。
	email *notify.EmailEventProducer
	log   *slog.Logger
}

func newRenewalFailureNotifier(db *sql.DB, store *notify.Store, email *notify.EmailEventProducer, log *slog.Logger) *renewalFailureNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &renewalFailureNotifier{db: db, store: store, email: email, log: log}
}

func (n *renewalFailureNotifier) NotifyRenewalFailure(ctx context.Context,
	notice billing.RenewalFailureNotice) error {

	lang := "en"
	if err := n.db.QueryRowContext(ctx,
		`SELECT language FROM users WHERE id = $1`, notice.UserID).Scan(&lang); err != nil {
		// 用户语言读不到（用户已删/DB 抖动）时仍发英文通知——资金事件不能
		// 因为展示层查询失败而静默丢弃。
		n.log.Warn("auto-renew notification: read user language failed",
			"user_id", notice.UserID, "error", err)
		lang = "en"
	}
	title, body := renewalFailureCopy(lang, notice)
	payload, err := json.Marshal(notify.InAppPayload{
		Title:   title,
		Body:    body,
		LinkURL: "/wallet",
	})
	if err != nil {
		return fmt.Errorf("platform: marshal renewal notification: %w", err)
	}
	userID := notice.UserID
	_, inAppErr := n.store.InsertInAppDeliveries(ctx, []notify.Delivery{{
		Category:        "billing.payment_failed",
		Channel:         notify.ChannelInApp,
		Recipient:       fmt.Sprintf("user:%d", notice.UserID),
		RecipientUserID: &userID,
		DedupeKey:       notice.DedupeKey,
		Status:          "sent",
		Attempt:         1,
		Payload:         payload,
	}})
	// W10-EM1：自动续费失败是事务性资金事件（billing.payment_failed 在
	// R-W10-06 的恒发集合内），有邮箱就发，不看用户偏好。邮件与站内信是
	// 两条独立队列：站内写入失败不应连累邮件；邮件失败也不改变站内结果。
	if n.email != nil {
		if _, emailErr := n.email.EnqueuePaymentFailed(ctx, notify.PaymentFailedEmailEvent{
			UserID:      notice.UserID,
			AmountLabel: "—",
			// 自动续费没有面向用户的订单号；用订阅 id 作为客服追溯键，
			// 避免把内部扣款凭证（payment method token）写进通知。
			OrderNo:     fmt.Sprintf("sub-%d", notice.SubscriptionID),
			ReasonLabel: renewalFailureReasonLabel(lang, notice.Reason),
			FailedAt:    time.Now(),
			DedupeKey:   notice.DedupeKey,
		}); emailErr != nil {
			n.log.Warn("auto-renew notification: enqueue email failed",
				"user_id", notice.UserID, "error", emailErr)
		}
	}
	if inAppErr != nil {
		return fmt.Errorf("platform: insert renewal notification: %w", inAppErr)
	}
	return nil
}

// renewalFailureReasonLabel 返回受控失败码的安全文案（7 语）。
//
// 只翻译 code 类别，绝不回显扣款渠道/上游错误原文（可能含卡信息）。
func renewalFailureReasonLabel(lang, reason string) string {
	labels := map[string]map[string]string{
		"en": {
			"plan_unavailable": "The subscription plan is no longer available",
			"plan_invalid":     "The subscription plan is invalid",
			"payment_config":   "Payment configuration is unavailable",
		},
		"zh-CN": {
			"plan_unavailable": "订阅套餐已不可用",
			"plan_invalid":     "订阅套餐配置无效",
			"payment_config":   "支付配置不可用",
		},
		"zh-TW": {
			"plan_unavailable": "訂閱方案已無法使用",
			"plan_invalid":     "訂閱方案設定無效",
			"payment_config":   "付款設定無法使用",
		},
		"fr": {
			"plan_unavailable": "Le forfait d'abonnement n'est plus disponible",
			"plan_invalid":     "Le forfait d'abonnement est invalide",
			"payment_config":   "La configuration de paiement est indisponible",
		},
		"ru": {
			"plan_unavailable": "Тарифный план больше недоступен",
			"plan_invalid":     "Некорректный тарифный план",
			"payment_config":   "Конфигурация платежей недоступна",
		},
		"ja": {
			"plan_unavailable": "サブスクリプションプランは利用できません",
			"plan_invalid":     "サブスクリプションプランが無効です",
			"payment_config":   "支払い設定を利用できません",
		},
		"vi": {
			"plan_unavailable": "Gói đăng ký không còn khả dụng",
			"plan_invalid":     "Gói đăng ký không hợp lệ",
			"payment_config":   "Cấu hình thanh toán không khả dụng",
		},
	}
	byLang, ok := labels[lang]
	if !ok {
		byLang = labels["en"]
	}
	if label, ok := byLang[reason]; ok {
		return label
	}
	switch lang {
	case "zh-CN":
		return "扣款未能完成"
	case "zh-TW":
		return "扣款未能完成"
	case "fr":
		return "Le paiement n'a pas pu être effectué"
	case "ru":
		return "Не удалось выполнить списание"
	case "ja":
		return "請求を完了できませんでした"
	case "vi":
		return "Không thể hoàn tất thanh toán"
	default:
		return "The payment could not be completed"
	}
}

// renewalFailureCopy 返回 (title, body)。只使用受控原因码的安全文案；
// 绝不拼渠道原始错误（可能回显卡信息）。7 语与 notify.DefaultLocale 同集。
func renewalFailureCopy(lang string, notice billing.RenewalFailureNotice) (string, string) {
	type copy struct{ title, body string }
	copies := map[string]copy{
		"en": {
			"Subscription auto-renewal failed",
			"We could not charge your saved payment method for subscription auto-renewal. Please update your payment method.",
		},
		"zh-CN": {
			"订阅自动续费失败",
			"订阅自动续费扣款失败，请更新支付方式后重试。",
		},
		"zh-TW": {
			"訂閱自動續費失敗",
			"訂閱自動續費扣款失敗，請更新付款方式後重試。",
		},
		"fr": {
			"Échec du renouvellement automatique",
			"Le paiement avec votre moyen de paiement enregistré a échoué. Veuillez le mettre à jour.",
		},
		"ru": {
			"Не удалось продлить подписку автоматически",
			"Не удалось списать средства с сохранённого способа оплаты. Обновите способ оплаты.",
		},
		"ja": {
			"サブスクリプションの自動更新に失敗しました",
			"保存された支払い方法での請求に失敗しました。支払い方法を更新してください。",
		},
		"vi": {
			"Gia hạn tự động thất bại",
			"Không thể tính tiền bằng phương thức thanh toán đã lưu. Vui lòng cập nhật phương thức thanh toán.",
		},
	}
	selected, ok := copies[lang]
	if !ok {
		selected = copies["en"]
	}
	body := selected.body
	switch lang {
	case "zh-CN":
		if notice.Disabled {
			body += "自动续费已关闭。"
		}
	case "zh-TW":
		if notice.Disabled {
			body += "自動續費已關閉。"
		}
	case "fr":
		if notice.Disabled {
			body += " Le renouvellement automatique a été désactivé."
		}
	case "ru":
		if notice.Disabled {
			body += " Автопродление отключено."
		}
	case "ja":
		if notice.Disabled {
			body += "自動更新は無効になりました。"
		}
	case "vi":
		if notice.Disabled {
			body += " Gia hạn tự động đã bị tắt."
		}
	default:
		if notice.Disabled {
			body += " Automatic renewal has been turned off."
		}
	}
	return selected.title, body
}
