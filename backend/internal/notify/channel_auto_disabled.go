package notify

// W7/M4-a：渠道自动禁用的站内通知直投（R-M4-09 的 C 节奏）。
//
// 为什么单独一个文件：投递语义与公告广播不同——收件人是**全体启用管理员**
// （不是全体用户），dedupe 前缀来自一次状态迁移（updated_at），内容只含
// 稳定 code 的本地化文案。把它塞进 DeliverAnnouncementInApp 会让两条
// 收件人/去重语义纠缠在一起。
//
// email 不在 M4 范围（R-M4-18）：这里只写 channel='inapp' 的投递行，
// 不触碰 SMTP/队列 worker。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ChannelAutoDisabledInput 是一次自动禁用事件的站内通知输入。
type ChannelAutoDisabledInput struct {
	ChannelID   int64
	ChannelName string
	// ReasonCode 是稳定 code（channel_upstream_error / channel_auth_error /
	// channel_keyword / channel_timeout），由 channelhealth 判定产生。
	ReasonCode string
	DisabledAt time.Time
	// ChannelsURL 是通知里的跳转链接（可为相对路径）。
	ChannelsURL string
	// DedupeKey 是**不含用户 id** 的事件前缀；投递时追加 `:<user_id>`，
	// 保证同一迁移对每个管理员恰好一条、重复投递幂等。
	DedupeKey string
}

// channelAutoDisabledTitles / reason labels 是站内文案的代码真源。
//
// 站内投递的 payload 是静态快照，因此本地化必须在**生产端**完成（按每个
// 管理员的 users.language）；不能像 UI 那样把 code 留给前端映射。email 模板
// 仍由 admin_username 等变量单独渲染，与此处无关。
var channelAutoDisabledTitles = map[Locale]string{
	LocaleEN:   "Channel auto-disabled",
	LocaleZHCN: "渠道已自动禁用",
	LocaleZHTW: "渠道已自動停用",
	LocaleFR:   "Canal désactivé automatiquement",
	LocaleRU:   "Канал автоматически отключён",
	LocaleJA:   "チャネルが自動無効化されました",
	LocaleVI:   "Kênh đã bị tự động vô hiệu hóa",
}

var channelAutoDisabledReasonLabels = map[Locale]map[string]string{
	LocaleEN: {
		"channel_upstream_error": "Upstream error",
		"channel_auth_error":     "Upstream authentication error",
		"channel_keyword":        "Response matched a disable keyword",
		"channel_timeout":        "Response timed out",
	},
	LocaleZHCN: {
		"channel_upstream_error": "上游错误",
		"channel_auth_error":     "上游鉴权错误",
		"channel_keyword":        "响应命中禁用关键词",
		"channel_timeout":        "响应超时",
	},
	LocaleZHTW: {
		"channel_upstream_error": "上游錯誤",
		"channel_auth_error":     "上游驗證失敗",
		"channel_keyword":        "回應命中停用關鍵字",
		"channel_timeout":        "回應逾時",
	},
	LocaleFR: {
		"channel_upstream_error": "Erreur du service amont",
		"channel_auth_error":     "Erreur d'authentification amont",
		"channel_keyword":        "Réponse correspondant à un mot-clé de désactivation",
		"channel_timeout":        "Délai de réponse dépassé",
	},
	LocaleRU: {
		"channel_upstream_error": "Ошибка вышестоящего провайдера",
		"channel_auth_error":     "Ошибка аутентификации на стороне провайдера",
		"channel_keyword":        "Ответ содержит ключевое слово для отключения",
		"channel_timeout":        "Превышено время ожидания ответа",
	},
	LocaleJA: {
		"channel_upstream_error": "上流エラー",
		"channel_auth_error":     "上流認証エラー",
		"channel_keyword":        "応答が無効化キーワードに一致しました",
		"channel_timeout":        "応答がタイムアウトしました",
	},
	LocaleVI: {
		"channel_upstream_error": "Lỗi từ nhà cung cấp",
		"channel_auth_error":     "Lỗi xác thực từ nhà cung cấp",
		"channel_keyword":        "Phản hồi khớp từ khóa vô hiệu hóa",
		"channel_timeout":        "Phản hồi quá thời gian",
	},
}

// channelAutoDisabledCopy 返回某语言的标题与原因标签。未知 code 用
// 「未知原因」类的固定文案，绝不回显 code 以外的上游文本。
func channelAutoDisabledCopy(locale Locale, reasonCode string) (title, reasonLabel string) {
	if _, ok := channelAutoDisabledTitles[locale]; !ok {
		locale = DefaultLocale
	}
	title = channelAutoDisabledTitles[locale]
	labels := channelAutoDisabledReasonLabels[locale]
	reasonLabel, ok := labels[reasonCode]
	if !ok {
		switch locale {
		case LocaleZHCN:
			reasonLabel = "未知原因"
		case LocaleZHTW:
			reasonLabel = "未知原因"
		case LocaleFR:
			reasonLabel = "Raison inconnue"
		case LocaleRU:
			reasonLabel = "Неизвестная причина"
		case LocaleJA:
			reasonLabel = "不明な理由"
		case LocaleVI:
			reasonLabel = "Lý do không xác định"
		default:
			reasonLabel = "Unknown reason"
		}
	}
	return title, reasonLabel
}

// DeliverChannelAutoDisabledInApp 为每个启用管理员生成一条
// `ops.channel_auto_disabled` 站内投递记录（R-M4-09）。
//
// 幂等：dedupe_key = `<input.DedupeKey>:<user_id>`，ON CONFLICT DO NOTHING；
// 同一次状态迁移重试只增加 Duplicates，不会产生第二条未读。返回实际插入行数。
//
// best-effort：模板停用/渲染失败的单个用户跳过并 Warn，不中断整批；存储错误
// 返回给调用方（channelhealth.Reporter 只 Warn，不回滚已提交的状态迁移）。
// 与公告广播不同，收件人查询按 role='admin' 过滤：自动禁用是运维事件，普通
// 用户不应看到渠道拓扑与故障信息。
func (s *Service) DeliverChannelAutoDisabledInApp(ctx context.Context, in ChannelAutoDisabledInput) (int, error) {
	if in.ChannelID <= 0 {
		return 0, errors.New("notify: channel id is required")
	}
	if strings.TrimSpace(in.DedupeKey) == "" {
		return 0, errors.New("notify: channel auto-disabled dedupe key is required")
	}
	spec, ok := LookupTemplate("ops.channel_auto_disabled", ChannelInApp)
	if !ok {
		// 注册表是编译期常量；走到这里说明代码被改坏（R-M4-09 要求该组存在）。
		return 0, errors.New("notify: ops.channel_auto_disabled inapp template is not registered")
	}

	baseVars := map[string]any{
		"channel_name": in.ChannelName,
		"channel_id":   strconv.FormatInt(in.ChannelID, 10),
		"disabled_at":  in.DisabledAt.UTC().Format(time.RFC3339),
		"channels_url": in.ChannelsURL,
	}
	// 按语言缓存渲染结果：管理员数量通常个位数，但同语言只需渲染一次。
	type renderedNotice struct {
		payload []byte
	}
	cache := make(map[Locale]renderedNotice)
	renderFor := func(locale Locale) (renderedNotice, bool) {
		if cached, ok := cache[locale]; ok {
			return cached, true
		}
		title, reasonLabel := channelAutoDisabledCopy(locale, in.ReasonCode)
		vars := make(map[string]any, len(baseVars)+2)
		for key, value := range baseVars {
			vars[key] = value
		}
		vars["title"] = title
		vars["reason_label"] = reasonLabel

		// allowBundle=true：模板行尚未播种时用内置 bundle 渲染（直投是
		// best-effort，不能因为一次播种缺失就丢掉运维通知）。
		content, resolved, row, err := s.resolve(ctx, spec, locale, true)
		if err != nil {
			s.log.Warn("notify: resolve channel_auto_disabled template failed",
				"locale", locale, "err", err)
			return renderedNotice{}, false
		}
		if row != nil && !row.Enabled {
			s.log.Warn("notify: channel_auto_disabled template is disabled", "locale", locale)
			return renderedNotice{}, false
		}
		rendered, err := RenderContent(spec, content, resolved, vars)
		if err != nil {
			s.log.Warn("notify: render channel_auto_disabled template failed",
				"locale", locale, "err", err)
			return renderedNotice{}, false
		}
		payload, err := json.Marshal(InAppPayload{
			Title:   title,
			Body:    rendered.Body,
			LinkURL: in.ChannelsURL,
		})
		if err != nil {
			s.log.Warn("notify: marshal channel_auto_disabled payload failed",
				"locale", locale, "err", err)
			return renderedNotice{}, false
		}
		notice := renderedNotice{payload: payload}
		cache[locale] = notice
		return notice, true
	}

	const batchSize = 500
	delivered := 0
	var afterID int64
	for {
		audience, err := s.store.ListAdminInAppAudience(ctx, afterID, batchSize)
		if err != nil {
			return delivered, err
		}
		if len(audience) == 0 {
			return delivered, nil
		}
		batch := make([]Delivery, 0, len(audience))
		for _, admin := range audience {
			notice, ok := renderFor(admin.Locale)
			if !ok {
				continue
			}
			userID := admin.UserID
			localeCopy := admin.Locale
			key := spec.Key
			batch = append(batch, Delivery{
				TemplateKey:     &key,
				Category:        spec.Category,
				Channel:         ChannelInApp,
				Recipient:       "user:" + strconv.FormatInt(userID, 10),
				RecipientUserID: &userID,
				Locale:          &localeCopy,
				DedupeKey:       fmt.Sprintf("%s:%d", in.DedupeKey, userID),
				Status:          "sent",
				Attempt:         1,
				Payload:         notice.payload,
			})
		}
		if len(batch) > 0 {
			inserted, err := s.store.InsertInAppDeliveries(ctx, batch)
			if err != nil {
				return delivered, err
			}
			delivered += inserted
		}
		if len(audience) < batchSize {
			return delivered, nil
		}
		afterID = audience[len(audience)-1].UserID
	}
}
