package mj

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/tasks"
)

// 本文件是图片代理（/mj/image/{id}）的签名与产物地址投影（W8/MJ-c）。
//
// 冻结口径（R-MJ-05/09/10/11）：
//   - 签名绑定 `mj_id ‖ owner_user_id ‖ exp`，任何一项变化都会让 HMAC 失效；
//   - 签名密钥从 CREDENTIAL_ENCRYPTION_KEY 派生，但使用**独立域前缀**
//     `newtg/mj-image/v1`——与产物票据（newtg/task-artifact-content/v1）分开，
//     避免同一主密钥上的签名被搬到另一种语义里重放；
//   - TTL 300s，查询参数 `sig`/`exp`；
//   - 校验用 hmac.Equal（常量时间），不泄露「哪一位不对」。

const (
	// ImageURLTTL 是图片签名 URL 的有效期（relay-05 §1.3：CAPURL_TTL=300s）。
	ImageURLTTL = 300 * time.Second

	// ImageSigParam / ImageExpParam 是签名 URL 的查询参数名（R-MJ-09 冻结）。
	ImageSigParam = "sig"
	ImageExpParam = "exp"

	// imageSignMessagePrefix 是 HMAC 的域分隔前缀。
	//
	// 没有域分隔时，同一把主密钥签发的产物票据可能被当作图片票据使用：
	// 两者都是 base64url(HMAC-SHA256)，仅靠参数名区分不了。前缀把签名
	// 绑定到「图片代理」这一个用途（与 tasks 包的 artifactAccessMessagePrefix
	// 同形、不同值）。
	imageSignMessagePrefix = "newtg/mj-image/v1\x00"
)

// ImageSigner 签发/校验图片能力 URL。可并发使用。
//
// 与 tasks.ArtifactSigner 分开实现而不是复用：两者的消息字段、TTL 边界与
// 失败语义都不同（图片要求 404、产物票据是 403），共用一套会让一次改动
// 同时改变两个端点族的授权语义。
type ImageSigner struct {
	key []byte
}

// NewImageSigner 从主密钥派生图片签名密钥。
//
// 派生（SHA-256(域前缀 ‖ secret)）而不是直接用主密钥：轮换
// CREDENTIAL_ENCRYPTION_KEY 即可整体撤销所有已签发图片 URL，且图片域与
// 渠道凭据加密域之间不共享可用密钥材料。
func NewImageSigner(secret string) *ImageSigner {
	sum := sha256.Sum256([]byte(imageSignMessagePrefix + secret))
	return &ImageSigner{key: sum[:]}
}

// Sign 为一个 (MjId, owner) 签发图片票据。
//
// 非法输入返回 ("", 0)：调用方拿不到可用地址，而不是生成一个永远验不过的
// URL（那会让排障误以为是密钥/时钟问题）。
func (s *ImageSigner) Sign(mjID string, ownerUserID int64, now time.Time) (string, int64) {
	if s == nil || ownerUserID <= 0 || !ValidMjID(mjID) {
		return "", 0
	}
	expiry := now.Unix() + int64(ImageURLTTL/time.Second)
	return base64.RawURLEncoding.EncodeToString(s.mac(mjID, ownerUserID, expiry)), expiry
}

// Verify 校验票据。任何格式错误、签名不符或已过期都返回 false。
//
// 顺序是「先验签、后看时效」：先证明票据是我们签的，再讨论它是否过期，
// 避免未签名输入触发差异化错误路径。比较用 hmac.Equal（常量时间），
// 不以字节是否相同来缩短比较。
func (s *ImageSigner) Verify(mjID string, ownerUserID int64, expiry int64, sig string, now time.Time) bool {
	if s == nil || ownerUserID <= 0 || expiry <= 0 || sig == "" || !ValidMjID(mjID) {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	want := s.mac(mjID, ownerUserID, expiry)
	if !hmac.Equal(got, want) {
		return false
	}
	// exp 是绝对 Unix 秒：exp 当秒仍有效（与签发时刻 now+TTL 对齐）。
	return now.Unix() <= expiry
}

// ParseImageTicket 解析查询串里的 `sig`/`exp`。
//
// 只做形状解析，不验签（验签需要候选 owner，只有 DB 能给出）。缺失、
// 非数字、非正的 exp 或空 sig 都返回 ok=false；调用方统一映射 404，
// 不区分「缺失」与「畸形」，避免响应成为参数枚举 oracle。
func ParseImageTicket(query url.Values) (expiry int64, sig string, ok bool) {
	if query == nil {
		return 0, "", false
	}
	expRaw := strings.TrimSpace(query.Get(ImageExpParam))
	sig = strings.TrimSpace(query.Get(ImageSigParam))
	if expRaw == "" || sig == "" {
		return 0, "", false
	}
	parsed, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, "", false
	}
	return parsed, sig, true
}

// mac 计算 HMAC-SHA256(域前缀 ‖ mjID ‖ owner ‖ exp)。
//
// 字段之间用 NUL 分隔：任取两个字段都含不了 NUL，拼接不会产生歧义
// （例如 mjID "a"+owner "12" 与 mjID "a1"+owner "2" 签名相同）。
func (s *ImageSigner) mac(mjID string, ownerUserID int64, expiry int64) []byte {
	m := hmac.New(sha256.New, s.key)
	io.WriteString(m, imageSignMessagePrefix)
	io.WriteString(m, mjID)
	m.Write([]byte{0})
	io.WriteString(m, strconv.FormatInt(ownerUserID, 10))
	m.Write([]byte{0})
	io.WriteString(m, strconv.FormatInt(expiry, 10))
	return m.Sum(nil)
}

// ImageURL 从任务行的 data 投影图片地址。
//
// 只接受非空字符串：上游快照里的 image_url 可能是 null/对象/数字（不同
// proxy 版本的畸形响应），把它们当地址拼进请求只会得到不可诊断的出站
// 失败。缺失/非法由调用方映射为 502（存储的 URL 不可用），而不是 404。
func ImageURL(task *tasks.Task) (string, bool) {
	if task == nil || task.Data == nil {
		return "", false
	}
	raw, ok := task.Data[DataImageURL]
	if !ok {
		return "", false
	}
	text, ok := raw.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	return text, true
}
