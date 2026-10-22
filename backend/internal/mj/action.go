package mj

import (
	"strconv"
	"strings"
)

// Action 是绘图任务族的族内动作标识（relay-05 §3.2，18 个）。
//
// 【为什么不复用插件动作枚举】TaskHandle.Action 的解释权归 Family
// （relay-05:210-236）：同一字面量在 plugin 与 mj 两族含义不同，跨族
// 合并会让渠道选择、计价与轮询分派互相串味。本类型只在本包使用。
type Action string

const (
	ActionImagine       Action = "IMAGINE"
	ActionDescribe      Action = "DESCRIBE"
	ActionBlend         Action = "BLEND"
	ActionUpscale       Action = "UPSCALE"
	ActionVariation     Action = "VARIATION"
	ActionReroll        Action = "REROLL"
	ActionInpaint       Action = "INPAINT"
	ActionModal         Action = "MODAL"
	ActionZoom          Action = "ZOOM"
	ActionCustomZoom    Action = "CUSTOM_ZOOM"
	ActionShorten       Action = "SHORTEN"
	ActionHighVariation Action = "HIGH_VARIATION"
	ActionLowVariation  Action = "LOW_VARIATION"
	ActionPan           Action = "PAN"
	ActionSwapFace      Action = "SWAP_FACE"
	ActionUpload        Action = "UPLOAD"
	ActionVideo         Action = "VIDEO"
	ActionEdits         Action = "EDITS"
)

// actionModels 是动作 → 模型名的唯一权威表（relay-05 §3.2）。
//
// 这张表同时驱动渠道选择（按 mj_<action> 找渠道）、/v1/models 下发与
// 计价查表；任何新增/改名都必须改这里，且不得在别处再维护第二份映射。
// SWAP_FACE 是唯一没有 mj_ 前缀的特例（模型名 swap_face）。
var actionModels = map[Action]string{
	ActionImagine:       "mj_imagine",
	ActionDescribe:      "mj_describe",
	ActionBlend:         "mj_blend",
	ActionUpscale:       "mj_upscale",
	ActionVariation:     "mj_variation",
	ActionReroll:        "mj_reroll",
	ActionInpaint:       "mj_inpaint",
	ActionModal:         "mj_modal",
	ActionZoom:          "mj_zoom",
	ActionCustomZoom:    "mj_custom_zoom",
	ActionShorten:       "mj_shorten",
	ActionHighVariation: "mj_high_variation",
	ActionLowVariation:  "mj_low_variation",
	ActionPan:           "mj_pan",
	ActionSwapFace:      "swap_face",
	ActionUpload:        "mj_upload",
	ActionVideo:         "mj_video",
	ActionEdits:         "mj_edits",
}

// PriceTier 是动作的计费档位（relay-05 §7.2 的四档冻结分类）。
//
// 【档位不是金额】设计把 18 个动作齐全地归入四档，但单价「落 model_pricing、
// 金额为配置项」——基线参考价（0.8/0.1/0.05/0）明确「不写入实现」。因此本
// 类型只承载「哪些动作免费、其余按次价查表」，绝不在代码里写死金额。
//
// 【为什么档位在领域包】渠道选择、/v1/models 与计价都从动作表派生；把档位
// 放在 HTTP 层会让第二个调用方（轮询/将来的管理面）再维护一份映射，档位
// 漂移会直接变成错价。
type PriceTier int

const (
	// TierFree 是免计费档位：INPAINT、CUSTOM_ZOOM（relay-05 §7.2 档位 1）。
	// 这两个动作不产生扣费（consumeQuota=false），不需要定价行。
	TierFree PriceTier = 1
	// TierLow 是低价档：DESCRIBE、UPSCALE、SWAP_FACE、UPLOAD。
	TierLow PriceTier = 2
	// TierStandard 是标准档：IMAGINE、BLEND、VARIATION、REROLL、MODAL、
	// ZOOM、SHORTEN、HIGH_VARIATION、LOW_VARIATION、PAN、EDITS。
	TierStandard PriceTier = 3
	// TierVideo 是视频档：VIDEO（唯一的高价档）。
	TierVideo PriceTier = 4
)

// actionTiers 是动作 → 档位的唯一权威表（relay-05 §7.2）。
//
// 与 actionModels 并列维护而不是合并成一个结构：模型名驱动渠道选择，档位
// 驱动计费，两者任一缺失都必须在编译/测试期被发现（测试断言 18 个动作全
// 覆盖，多一条少一条都会红）。档位只决定「是否查价」，金额仍由配置决定。
var actionTiers = map[Action]PriceTier{
	ActionImagine:       TierStandard,
	ActionDescribe:      TierLow,
	ActionBlend:         TierStandard,
	ActionUpscale:       TierLow,
	ActionVariation:     TierStandard,
	ActionReroll:        TierStandard,
	ActionInpaint:       TierFree,
	ActionModal:         TierStandard,
	ActionZoom:          TierStandard,
	ActionCustomZoom:    TierFree,
	ActionShorten:       TierStandard,
	ActionHighVariation: TierStandard,
	ActionLowVariation:  TierStandard,
	ActionPan:           TierStandard,
	ActionSwapFace:      TierLow,
	ActionUpload:        TierLow,
	ActionVideo:         TierVideo,
	ActionEdits:         TierStandard,
}

// PriceTier 返回动作的计费档位。未知动作返回 false——调用方必须拒绝，不能
// 猜成免费档（那会把任意未知动作变成白送）。
func (a Action) PriceTier() (PriceTier, bool) {
	tier, ok := actionTiers[a]
	return tier, ok
}

// Free 报告档位是否免计费。
func (t PriceTier) Free() bool { return t == TierFree }

// actionOrder 是 18 个动作的稳定遍历顺序（relay-05 §3.2 的表序）。
//
// 与 actionModels 分开保存：map 的迭代顺序不稳定，而下发 /v1/models、
// 生成文档与测试快照都需要一个确定的顺序。
var actionOrder = []Action{
	ActionImagine,
	ActionDescribe,
	ActionBlend,
	ActionUpscale,
	ActionVariation,
	ActionReroll,
	ActionInpaint,
	ActionModal,
	ActionZoom,
	ActionCustomZoom,
	ActionShorten,
	ActionHighVariation,
	ActionLowVariation,
	ActionPan,
	ActionSwapFace,
	ActionUpload,
	ActionVideo,
	ActionEdits,
}

// Actions 返回冻结的 18 个动作副本。
//
// 返回副本而不是导出切片：a2 若原地排序/截断共享切片，会悄悄改变本包的
// 权威动作表（以及依赖它的 /v1/models 与计价遍历顺序）。
func Actions() []Action {
	out := make([]Action, len(actionOrder))
	copy(out, actionOrder)
	return out
}

// Model 返回动作对应的模型名。未知动作返回 false——调用方必须拒绝而不是
// 拿空模型名去选渠道（空模型会退化成「任意渠道」）。
func (a Action) Model() (string, bool) {
	model, ok := actionModels[a]
	return model, ok
}

// Valid 报告动作是否属于冻结的 18 个族内动作。
func (a Action) Valid() bool {
	_, ok := actionModels[a]
	return ok
}

// ParseAction 把客户端/配置给出的动作字面量解析为 Action。
//
// 大小写不敏感：基线 change 端点接受 "UPSCALE" 等大写值，但不同客户端
// 存在小写写法；动作表本身不含大小写语义，归一化的代价为零。
func ParseAction(raw string) (Action, bool) {
	action := Action(strings.ToUpper(strings.TrimSpace(raw)))
	if !action.Valid() {
		return "", false
	}
	return action, true
}

// Endpoint 是 MJ-a 的 12 条提交端点（relay-05 §1.1）。
//
// 值就是 /mj/ 之后的路径，既当稳定标识又便于 a2 直接注册；端点本身不含
// 动作——动作由路径或入参解析（change/action/simple-change）。
type Endpoint string

const (
	EndpointImagine       Endpoint = "submit/imagine"
	EndpointChange        Endpoint = "submit/change"
	EndpointSimpleChange  Endpoint = "submit/simple-change"
	EndpointAction        Endpoint = "submit/action"
	EndpointModal         Endpoint = "submit/modal"
	EndpointShorten       Endpoint = "submit/shorten"
	EndpointDescribe      Endpoint = "submit/describe"
	EndpointBlend         Endpoint = "submit/blend"
	EndpointEdits         Endpoint = "submit/edits"
	EndpointVideo         Endpoint = "submit/video"
	EndpointUploadDiscord Endpoint = "submit/upload-discord-images"
	EndpointInsightFace   Endpoint = "insight-face/swap"
)

// endpoints 是 12 条提交端点的稳定顺序清单。
var endpoints = []Endpoint{
	EndpointImagine,
	EndpointChange,
	EndpointSimpleChange,
	EndpointAction,
	EndpointModal,
	EndpointShorten,
	EndpointDescribe,
	EndpointBlend,
	EndpointEdits,
	EndpointVideo,
	EndpointUploadDiscord,
	EndpointInsightFace,
}

// Endpoints 返回 12 条提交端点的副本（供 a2 注册与测试遍历）。
//
// 返回副本的理由同 Actions：调用方原地修改共享切片会破坏所有使用方的
// 端点集合，而这正是 a2 路由注册的依据。
func Endpoints() []Endpoint {
	out := make([]Endpoint, len(endpoints))
	copy(out, endpoints)
	return out
}

// Path 返回端点的对外路径。
func (e Endpoint) Path() string { return "/mj/" + string(e) }

// Valid 报告端点是否属于 12 条冻结提交端点。
func (e Endpoint) Valid() bool {
	switch e {
	case EndpointImagine, EndpointChange, EndpointSimpleChange, EndpointAction,
		EndpointModal, EndpointShorten, EndpointDescribe, EndpointBlend,
		EndpointEdits, EndpointVideo, EndpointUploadDiscord, EndpointInsightFace:
		return true
	}
	return false
}

// RequiresPlus 报告端点是否只在 midjourney_plus 渠道可用（relay-05 §2.4）。
//
// Plus-only 是**端点能力**，不是动作能力：action 端点可解析出普通动作，
// 但它本身依赖 Plus 渠道的 customId 交互，因此仍必须走 Plus 渠道。
func RequiresPlus(e Endpoint) bool {
	switch e {
	case EndpointAction, EndpointModal, EndpointShorten, EndpointInsightFace:
		return true
	}
	return false
}

// LocksOrigin 报告该端点是否必须锁定原任务所属渠道（relay-05 §4.1）。
//
// 携带 taskId 的动作类端点强制锁定；action 的 taskId 是可选字段，只有
// 实际提供时才锁定。edits 虽然生成新图，但设计明确「无 taskId，不锁定」。
func LocksOrigin(e Endpoint, taskID string) bool {
	switch e {
	case EndpointChange, EndpointSimpleChange, EndpointModal, EndpointVideo:
		return true
	case EndpointAction:
		return strings.TrimSpace(taskID) != ""
	}
	return false
}

// ResolvedAction 是一次提交解析出的动作与索引。
//
// Index 的取值按解析路径不同：
//   - customId 的 upsample/variation 取按钮索引；customId 的其余动作
//     （low/high variation、pan、reroll、Outpaint、CustomZoom、Inpaint）
//     固定为 1；
//   - simple-change 的 `r` 为 0（基线 r 不带索引），u/v 取 1–4；
//   - 固定动作端点为 0（不适用）。
type ResolvedAction struct {
	Action Action
	Index  int
}

// ResolveAction 把端点与入参解析成族内动作（relay-05 §2.2/§3.2）。
//
// 固定动作端点直接返回；change 取 action 字段（video 强制 VIDEO）；
// simple-change 解析 content；action 端点解析 customId。未知动作一律
// unknown_action（HTTP 400），绝不落到空模型名继续执行。
func ResolveAction(e Endpoint, req *SubmitRequest) (ResolvedAction, *Error) {
	if !e.Valid() {
		return ResolvedAction{}, BadRequest(DescriptionUnknownEndpoint)
	}
	switch e {
	case EndpointImagine:
		return ResolvedAction{Action: ActionImagine}, nil
	case EndpointDescribe:
		return ResolvedAction{Action: ActionDescribe}, nil
	case EndpointBlend:
		return ResolvedAction{Action: ActionBlend}, nil
	case EndpointShorten:
		return ResolvedAction{Action: ActionShorten}, nil
	case EndpointModal:
		return ResolvedAction{Action: ActionModal}, nil
	case EndpointEdits:
		return ResolvedAction{Action: ActionEdits}, nil
	case EndpointUploadDiscord:
		return ResolvedAction{Action: ActionUpload}, nil
	case EndpointInsightFace:
		return ResolvedAction{Action: ActionSwapFace}, nil
	case EndpointVideo:
		// 设计强制 VIDEO，忽略客户端 action 字段（relay-05 §2.2）。
		return ResolvedAction{Action: ActionVideo}, nil
	case EndpointChange:
		action, ok := ParseAction(ptrValue(req, func(r *SubmitRequest) *string { return r.Action }))
		if !ok {
			return ResolvedAction{}, BadRequest(DescriptionUnknownAction)
		}
		return ResolvedAction{Action: action, Index: ptrIntValue(req.Index)}, nil
	case EndpointSimpleChange:
		_, action, index, err := ParseSimpleChange(ptrValue(req, func(r *SubmitRequest) *string { return r.Content }))
		if err != nil {
			return ResolvedAction{}, err
		}
		return ResolvedAction{Action: action, Index: index}, nil
	case EndpointAction:
		action, index, err := ParseCustomID(ptrValue(req, func(r *SubmitRequest) *string { return r.CustomID }))
		if err != nil {
			return ResolvedAction{}, err
		}
		return ResolvedAction{Action: action, Index: index}, nil
	}
	return ResolvedAction{}, BadRequest(DescriptionUnknownEndpoint)
}

func ptrValue(req *SubmitRequest, pick func(*SubmitRequest) *string) string {
	if req == nil {
		return ""
	}
	if v := pick(req); v != nil {
		return *v
	}
	return ""
}

func ptrIntValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// ParseCustomID 解析 Plus action 端点的 customId（relay-05 §3.2）。
//
// 支持两种形态：
//   - MJ::JOB::<action>::<index>::<uuid>（标准按钮）
//   - MJ::<action>::...（Outpaint/CustomZoom/Inpaint 等专用按钮）
//
// 每个分支都必须显式返回动作；带索引的分支（upsample/variation）在索引
// 缺失或非数字时返回 index_parse_failed。未识别 token → unknown_action。
func ParseCustomID(customID string) (Action, int, *Error) {
	customID = strings.TrimSpace(customID)
	if customID == "" {
		return "", 0, BadRequest(DescriptionCustomIDRequired)
	}
	parts := strings.Split(customID, "::")
	if len(parts) < 2 {
		return "", 0, BadRequest(DescriptionUnknownAction)
	}
	// 标准按钮的第二段是 JOB；否则动作 token 就在第二段。
	idx := 1
	if parts[0] == "MJ" && parts[1] == "JOB" {
		idx = 2
	}
	if idx >= len(parts) || parts[idx] == "" {
		return "", 0, BadRequest(DescriptionUnknownAction)
	}
	token := parts[idx]
	nextIndex := func() (int, *Error) {
		if idx+1 >= len(parts) {
			return 0, BadRequest(DescriptionIndexParseFailed)
		}
		n, err := strconv.Atoi(parts[idx+1])
		if err != nil {
			return 0, BadRequest(DescriptionIndexParseFailed)
		}
		return n, nil
	}

	switch {
	case strings.Contains(token, "upsample"):
		n, err := nextIndex()
		if err != nil {
			return "", 0, err
		}
		return ActionUpscale, n, nil
	case token == "variation":
		n, err := nextIndex()
		if err != nil {
			return "", 0, err
		}
		return ActionVariation, n, nil
	case token == "low_variation":
		return ActionLowVariation, 1, nil
	case token == "high_variation":
		return ActionHighVariation, 1, nil
	case strings.Contains(token, "variation"):
		// 形如 micro_variation 的未知变体不得按 VARIATION 执行——动作决定
		// 计价档位与上游参数，猜错会产出用户没要求的图。
		return "", 0, BadRequest(DescriptionUnknownAction)
	case strings.Contains(token, "pan"):
		return ActionPan, 1, nil
	case strings.Contains(token, "reroll"):
		return ActionReroll, 1, nil
	case token == "Outpaint":
		return ActionZoom, 1, nil
	case token == "CustomZoom":
		return ActionCustomZoom, 1, nil
	case token == "Inpaint":
		return ActionInpaint, 1, nil
	}
	return "", 0, BadRequest(DescriptionUnknownAction)
}

// ParseSimpleChange 解析 simple-change 的 content：
// "<taskId> <u1..u4|v1..v4|r>"（relay-05 §2.2，甲类-3：让该分支真正可达）。
//
// 任何不合法形态统一 content_parse_failed（固定错误，不回显 content）。
// 只接受恰好两段、索引恰好一位 1–4；"u10" 不得被截成 u1。
func ParseSimpleChange(content string) (string, Action, int, *Error) {
	fields := strings.Fields(strings.TrimSpace(content))
	if len(fields) != 2 {
		return "", "", 0, BadRequest(DescriptionContentParseFailed)
	}
	taskID := fields[0]
	if !ValidMjID(taskID) {
		return "", "", 0, BadRequest(DescriptionContentParseFailed)
	}
	token := strings.ToLower(fields[1])
	if token == "r" {
		return taskID, ActionReroll, 0, nil
	}
	if len(token) != 2 || token[1] < '1' || token[1] > '4' {
		return "", "", 0, BadRequest(DescriptionContentParseFailed)
	}
	index := int(token[1] - '0')
	switch token[0] {
	case 'u':
		return taskID, ActionUpscale, index, nil
	case 'v':
		return taskID, ActionVariation, index, nil
	}
	return "", "", 0, BadRequest(DescriptionContentParseFailed)
}
