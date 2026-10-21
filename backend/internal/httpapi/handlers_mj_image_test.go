package httpapi

// W8/MJ-c 图片代理的单元验收（fake store + httptest 上游）。
//
// 覆盖冻结裁决：
//   - 签名 URL 的 (mj_id, owner, exp) 绑定与 300s TTL；
//   - 归属：本人/多位候选 owner 任一匹配/他人/不存在/畸形 id 一律 404；
//   - 候选上限：超出上限的 owner 不被验证；
//   - SSRF：私网/环回/元数据/重定向/rebinding 全部拒绝，mode=off 不豁免；
//   - 状态码：ErrBlocked→403、ErrUnresolved/上游非 2xx/非图片/超限→502；
//   - 成功流式透传图片字节与 Content-Type，鉴权失败仍是 MJ 信封。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// mjImagePNG 是一段最小 PNG 头，用于断言字节原样透传。
var mjImagePNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R'}

type mjImageEnv struct {
	t      *testing.T
	mux    *http.ServeMux
	auth   string
	store  *fakeMJStore
	signer *mj.ImageSigner
	now    time.Time
}

func newMJImageEnv(t *testing.T, store *fakeMJStore, signer *mj.ImageSigner,
	guard *egress.Guard, config fakeMJConfig) *mjImageEnv {
	t.Helper()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tokens, key42, _ := newMJTokens()
	handler := NewMJHandler(MJHandlerConfig{
		Store:       store,
		Channels:    &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{}},
		Sealer:      NewSecretSealer("mj-image-handler-key"),
		Tokens:      tokens,
		Billing:     func(*contract.RelayContext) contract.BillingSession { return newTaskBillingRecorder() },
		Config:      config,
		ImageSigner: signer,
		ImageGuard:  guard,
		Now:         func() time.Time { return now },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	return &mjImageEnv{t: t, mux: mux, auth: "Bearer " + key42, store: store, signer: signer, now: now}
}

func (e *mjImageEnv) addTask(userID int64, id, imageURL string) *tasks.Task {
	e.t.Helper()
	task := &tasks.Task{
		ID: userID, PublicTaskID: fmt.Sprintf("task_%d_%s", userID, id),
		UpstreamTaskID: id, Family: tasks.FamilyMJ, Action: string(mj.ActionImagine),
		UserID: userID, ChannelID: 1, State: tasks.StateSuccess,
		Data: map[string]any{mj.DataImageURL: imageURL},
	}
	e.store.byUserUpstream[mjUserUpstreamKey(userID, id)] = task
	return task
}

func (e *mjImageEnv) ticket(id string, owner int64) (sig string, exp int64) {
	e.t.Helper()
	sig, exp = e.signer.Sign(id, owner, e.now)
	if sig == "" {
		e.t.Fatalf("Sign(%q,%d) returned an empty ticket", id, owner)
	}
	return sig, exp
}

func (e *mjImageEnv) path(id, sig string, exp int64) string {
	query := url.Values{}
	query.Set(mj.ImageSigParam, sig)
	query.Set(mj.ImageExpParam, strconv.FormatInt(exp, 10))
	return "/mj/image/" + url.PathEscape(id) + "?" + query.Encode()
}

func (e *mjImageEnv) get(target string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", e.auth)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// permissiveImageGuard 放行 loopback/任意端口（httptest 只能监听 127.0.0.1），
// 但仍拒绝私网/元数据/保留段——与生产策略的差异仅限测试必需的两项。
func permissiveImageGuard() *egress.Guard {
	return egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAll,
		AllowLoopback: true, AllowAnyPort: true,
	})
}

// strictLoopbackGuard 拒绝 loopback：用于证明地址规则真的被执行。
func strictLoopbackGuard() *egress.Guard {
	return egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAll,
		AllowLoopback: false, AllowAnyPort: true,
	})
}

func newImageUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func decodeMJImageError(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %q (%v)", rec.Body.String(), err)
	}
	if _, ok := body["code"]; !ok {
		t.Fatalf("MJ error envelope missing code: %v", body)
	}
	if _, ok := body["description"]; !ok {
		t.Fatalf("MJ error envelope missing description: %v", body)
	}
	return body
}

func TestMJImageOwnershipAndAnyCandidateMatch(t *testing.T) {
	upstream := newImageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(mjImagePNG)
	})
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-owner-key")
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))
	env.addTask(42, "mj_shared", upstream.URL+"/img.png")
	env.addTask(43, "mj_shared", upstream.URL+"/img.png")

	t.Run("owner 42", func(t *testing.T) {
		sig, exp := env.ticket("mj_shared", 42)
		rec := env.get(env.path("mj_shared", sig, exp))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), mjImagePNG) {
			t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.Bytes())
		}
	})
	t.Run("owner 43 shares the same MjId", func(t *testing.T) {
		sig, exp := env.ticket("mj_shared", 43)
		rec := env.get(env.path("mj_shared", sig, exp))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), mjImagePNG) {
			t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.Bytes())
		}
	})
}

func TestMJImageNotFoundCases(t *testing.T) {
	upstream := newImageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(mjImagePNG)
	})
	store := newFakeMJStore()
	store.byUserUpstream = map[string]*tasks.Task{}
	signer := mj.NewImageSigner("mj-image-owner-key")
	strictSigner := mj.NewImageSigner("another-key")
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))
	env.addTask(42, "mj_owned", upstream.URL+"/img.png")
	sig, exp := env.ticket("mj_owned", 42)

	foreignSig, _ := strictSigner.Sign("mj_owned", 42, env.now)
	expiredSig, expiredExp := signer.Sign("mj_owned", 42, env.now.Add(-mj.ImageURLTTL-time.Second))
	if expiredSig == "" {
		t.Fatal("expired ticket fixture failed")
	}

	cases := []struct {
		name   string
		target string
	}{
		{"malformed id", env.path("bad!id", sig, exp)},
		{"oversized id", env.path(strings.Repeat("a", mj.MaxMjIDLen+1), sig, exp)},
		{"missing ticket", "/mj/image/mj_owned"},
		{"missing sig", "/mj/image/mj_owned?exp=" + strconv.FormatInt(exp, 10)},
		{"non numeric exp", "/mj/image/mj_owned?sig=" + url.QueryEscape(sig) + "&exp=soon"},
		{"tampered sig", env.path("mj_owned", flipSignature(sig), exp)},
		{"foreign key", env.path("mj_owned", foreignSig, exp)},
		{"expired", env.path("mj_owned", expiredSig, expiredExp)},
		{"non owner", env.path("mj_owned", mustSign(t, signer, "mj_owned", 99, env.now), expOf(env.now))},
		{"unknown id", env.path("mj_unknown", mustSign(t, signer, "mj_unknown", 42, env.now), expOf(env.now))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.get(tc.target)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
			}
			body := decodeMJImageError(t, rec)
			if int(body["code"].(float64)) != mj.CodeRequestError ||
				body["description"] != mj.DescriptionTaskNotFound {
				t.Fatalf("envelope = %v, want code 4 task_not_found", body)
			}
			if strings.Contains(rec.Body.String(), "mj_owned") || strings.Contains(rec.Body.String(), "mj_unknown") {
				t.Fatalf("404 body must not echo the MjId: %s", rec.Body.String())
			}
		})
	}
}

func TestMJImageCandidateLimit(t *testing.T) {
	upstream := newImageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(mjImagePNG)
	})
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-limit-key")
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))
	// 101 个 owner 共享同一 MjId；候选上限 100，按 user_id 升序截断。
	for userID := int64(1); userID <= 101; userID++ {
		env.addTask(userID, "mj_crowded", upstream.URL+"/img.png")
	}
	if len(store.byUserUpstream) != 101 {
		t.Fatalf("fixture rows = %d, want 101", len(store.byUserUpstream))
	}

	sig, exp := env.ticket("mj_crowded", 100)
	if rec := env.get(env.path("mj_crowded", sig, exp)); rec.Code != http.StatusOK {
		t.Fatalf("owner inside the candidate window: status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	sigLast, expLast := env.ticket("mj_crowded", 101)
	if rec := env.get(env.path("mj_crowded", sigLast, expLast)); rec.Code != http.StatusNotFound {
		t.Fatalf("owner beyond the candidate window: status = %d, want 404", rec.Code)
	}
}

func TestMJImageSSRFAlwaysApplies(t *testing.T) {
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-ssrf-key")
	guard := permissiveImageGuard()
	env := newMJImageEnv(t, store, signer, guard, mjConfigured(nil))

	privateURLs := map[string]string{
		"private":  "http://10.0.0.5/img.png",
		"metadata": "http://169.254.169.254/latest/meta-data/",
		"cgnat":    "http://100.64.0.1/img.png",
		"reserved": "http://240.0.0.1/img.png",
	}
	for name, target := range privateURLs {
		t.Run(name, func(t *testing.T) {
			env.addTask(42, "mj_"+name, target)
			sig, exp := env.ticket("mj_"+name, 42)
			rec := env.get(env.path("mj_"+name, sig, exp))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			body := decodeMJImageError(t, rec)
			if int(body["code"].(float64)) != mj.CodeRequestError {
				t.Fatalf("403 envelope code = %v", body["code"])
			}
			if strings.Contains(rec.Body.String(), "10.0.0.5") || strings.Contains(rec.Body.String(), "169.254") {
				t.Fatalf("403 body must not echo the target URL: %s", rec.Body.String())
			}
		})
	}

	t.Run("loopback rejected by address rule", func(t *testing.T) {
		upstream := newImageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(mjImagePNG)
		})
		strict := newMJImageEnv(t, store, signer, strictLoopbackGuard(), mjConfigured(nil))
		strict.addTask(42, "mj_loopback", upstream.URL+"/img.png")
		sig, exp := strict.ticket("mj_loopback", 42)
		rec := strict.get(strict.path("mj_loopback", sig, exp))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("ftp scheme rejected", func(t *testing.T) {
		env.addTask(42, "mj_ftp", "ftp://cdn.example/img.png")
		sig, exp := env.ticket("mj_ftp", 42)
		rec := env.get(env.path("mj_ftp", sig, exp))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

func TestMJImageModeOffDoesNotDisableAddressRules(t *testing.T) {
	upstream := newImageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(mjImagePNG)
	})
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-off-key")
	// 不注入 ImageGuard：让 handler 从 sec.ssrf.* 构造策略。
	config := mjConfigured(map[string]json.RawMessage{"sec.ssrf.mode": json.RawMessage(`"off"`)})
	env := newMJImageEnv(t, store, signer, nil, config)
	env.addTask(42, "mj_off", upstream.URL+"/img.png")
	sig, exp := env.ticket("mj_off", 42)
	rec := env.get(env.path("mj_off", sig, exp))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sec.ssrf.mode=off must not exempt /mj/image: status = %d, want 403 (body=%s)",
			rec.Code, rec.Body.String())
	}
}

func TestMJImagePolicyClampsConfig(t *testing.T) {
	config := fakeMJConfig{
		egress.KeyMode:           json.RawMessage(`"off"`),
		egress.KeyDomainMode:     json.RawMessage(`"allowlist"`),
		egress.KeyDomainList:     json.RawMessage(`["api.example"]`),
		egress.KeyAllowPrivateIP: json.RawMessage(`true`),
	}
	policy := mjImagePolicy(config)
	if policy.Mode != egress.ModeBasic {
		t.Fatalf("mode = %q, want basic (off clamped)", policy.Mode)
	}
	if policy.DomainMode != egress.DomainAll || len(policy.DomainList) != 0 {
		t.Fatalf("domain mode/list = %q/%v, want all/empty", policy.DomainMode, policy.DomainList)
	}
	if policy.AllowPrivateIP {
		t.Fatal("allow_private_ip must not widen /mj/image")
	}
}

func TestMJImageRebindingSecondResolutionIsBlocked(t *testing.T) {
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-rebind-key")
	var resolves int
	guard := egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAll, AllowAnyPort: true,
	}).WithResolver(func(_ context.Context, _ string) ([]net.IP, error) {
		resolves++
		if resolves == 1 {
			// ValidateURL 看到公网地址 → 通过。
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		// 拨号阶段重解析返回私网 → DialContext 必须拒绝（反 rebinding）。
		return []net.IP{net.ParseIP("10.0.0.7")}, nil
	})
	env := newMJImageEnv(t, store, signer, guard, mjConfigured(nil))
	env.addTask(42, "mj_rebind", "http://rebind.example/img.png")
	sig, exp := env.ticket("mj_rebind", 42)
	rec := env.get(env.path("mj_rebind", sig, exp))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (rebinding blocked at dial time)", rec.Code)
	}
	if resolves < 2 {
		t.Fatalf("resolver calls = %d, want at least 2 (pre-check + dial)", resolves)
	}
}

func TestMJImageUnresolvedHostIsBadGateway(t *testing.T) {
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-dns-key")
	guard := egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAll, AllowAnyPort: true,
	}).WithResolver(func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	})
	env := newMJImageEnv(t, store, signer, guard, mjConfigured(nil))
	env.addTask(42, "mj_dns", "http://missing.example/img.png")
	sig, exp := env.ticket("mj_dns", 42)
	rec := env.get(env.path("mj_dns", sig, exp))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestMJImageUpstreamFailures(t *testing.T) {
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-upstream-key")

	upstream := newImageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(mjImagePNG)
		case "/missing.png":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("upstream 404 body"))
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"not":"an image"}`))
		case "/huge.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", strconv.Itoa(maxMJImageBytes+1))
			w.WriteHeader(http.StatusOK)
		case "/redirect.png":
			http.Redirect(w, r, "/secret.png", http.StatusFound)
		case "/secret.png":
			// 若跟随重定向，这个端点会被访问；测试断言它不被访问。
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(mjImagePNG)
		case "/no-content-type":
			w.Header()["Content-Type"] = nil
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mjImagePNG)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	})
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))

	cases := []struct {
		name string
		path string
		want int
	}{
		{"upstream non-2xx", "/missing.png", http.StatusBadGateway},
		{"non image content type", "/json", http.StatusBadGateway},
		{"oversized body", "/huge.png", http.StatusBadGateway},
		{"redirect not followed", "/redirect.png", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "mj_" + strings.ReplaceAll(tc.name, " ", "_")
			env.addTask(42, id, upstream.URL+tc.path)
			sig, exp := env.ticket(id, 42)
			rec := env.get(env.path(id, sig, exp))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			body := decodeMJImageError(t, rec)
			if int(body["code"].(float64)) != mj.CodeUnknownError {
				t.Fatalf("502 envelope code = %v", body["code"])
			}
			if strings.Contains(rec.Body.String(), "upstream 404 body") {
				t.Fatalf("upstream error body must not be passed through")
			}
		})
	}

	t.Run("missing content type defaults to image/jpeg", func(t *testing.T) {
		env.addTask(42, "mj_no_ct", upstream.URL+"/no-content-type")
		sig, exp := env.ticket("mj_no_ct", 42)
		rec := env.get(env.path("mj_no_ct", sig, exp))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
			t.Fatalf("Content-Type = %q, want image/jpeg", got)
		}
	})
}

func TestMJImageStoredURLInvalidIsBadGateway(t *testing.T) {
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-url-key")
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))

	cases := []struct {
		name     string
		imageURL any
	}{
		{"empty", ""},
		{"relative", "/img.png"},
		{"non string", 123},
		{"missing", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "mj_url_" + strings.ReplaceAll(tc.name, " ", "_")
			task := env.addTask(42, id, "")
			if tc.imageURL == nil {
				delete(task.Data, mj.DataImageURL)
			} else {
				task.Data[mj.DataImageURL] = tc.imageURL
			}
			sig, exp := env.ticket(id, 42)
			rec := env.get(env.path(id, sig, exp))
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMJImageSuccessStreamsAndPreservesType(t *testing.T) {
	upstream := newImageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/webp; charset=binary")
		_, _ = w.Write(mjImagePNG)
	})
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-success-key")
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))
	env.addTask(42, "mj_ok", upstream.URL+"/img.webp")
	sig, exp := env.ticket("mj_ok", 42)

	rec := env.get(env.path("mj_ok", sig, exp))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), mjImagePNG) {
		t.Fatalf("body = %q, want the upstream bytes", rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("Content-Type = %q, want image/webp", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(mjImagePNG)) {
		t.Fatalf("Content-Length = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=60" {
		t.Fatalf("Cache-Control = %q", got)
	}

	t.Run("HEAD returns headers only", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, env.path("mj_ok", sig, exp), nil)
		req.Header.Set("Authorization", env.auth)
		head := httptest.NewRecorder()
		env.mux.ServeHTTP(head, req)
		if head.Code != http.StatusOK {
			t.Fatalf("HEAD status = %d", head.Code)
		}
		if head.Body.Len() != 0 {
			t.Fatalf("HEAD body = %q, want empty", head.Body.Bytes())
		}
		if got := head.Header().Get("Content-Type"); got != "image/webp" {
			t.Fatalf("HEAD Content-Type = %q", got)
		}
	})
}

// TestMJImageLimitReaderAllowsExactLimit 冻结「恰好等于上限」不是超限。
func TestMJImageLimitReaderAllowsExactLimit(t *testing.T) {
	exact := &mjImageLimitReader{body: strings.NewReader("abcd"), remaining: 4}
	got, err := io.ReadAll(exact)
	if err != nil || string(got) != "abcd" {
		t.Fatalf("exact-limit read = (%q,%v), want abcd/nil", got, err)
	}
	over := &mjImageLimitReader{body: strings.NewReader("abcde"), remaining: 4}
	if _, err := io.ReadAll(over); !errors.Is(err, errMJImageTooLarge) {
		t.Fatalf("over-limit read error = %v, want errMJImageTooLarge", err)
	}
}

func TestMJImageStoreErrorIsInternal(t *testing.T) {
	store := newFakeMJStore()
	store.lookupErr = errors.New("database down")
	signer := mj.NewImageSigner("mj-image-db-key")
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))
	sig, exp := env.ticket("mj_db", 42)
	rec := env.get(env.path("mj_db", sig, exp))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := decodeMJImageError(t, rec)
	if body["description"] != mj.DescriptionInternalError {
		t.Fatalf("description = %v", body["description"])
	}
}

func TestMJImageAuthFailureUsesMJEnvelope(t *testing.T) {
	store := newFakeMJStore()
	signer := mj.NewImageSigner("mj-image-auth-key")
	env := newMJImageEnv(t, store, signer, permissiveImageGuard(), mjConfigured(nil))
	sig, exp := env.ticket("mj_auth", 42)

	req := httptest.NewRequest(http.MethodGet, env.path("mj_auth", sig, exp), nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := decodeMJImageError(t, rec)
	if _, ok := body["error"]; ok {
		t.Fatalf("auth failure must use the MJ envelope, not the relay error body: %v", body)
	}
	if body["description"] != mj.DescriptionInvalidToken {
		t.Fatalf("description = %v", body["description"])
	}
}

// flipSignature 改掉签名最后一个字符，构造同长度但内容不同的票据。
func flipSignature(sig string) string {
	if sig == "" {
		return "x"
	}
	last := sig[len(sig)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return sig[:len(sig)-1] + string(replacement)
}

func mustSign(t *testing.T, signer *mj.ImageSigner, id string, owner int64, now time.Time) string {
	t.Helper()
	sig, _ := signer.Sign(id, owner, now)
	if sig == "" {
		t.Fatalf("Sign(%q,%d) failed", id, owner)
	}
	return sig
}

func expOf(now time.Time) int64 {
	return now.Unix() + int64(mj.ImageURLTTL/time.Second)
}
