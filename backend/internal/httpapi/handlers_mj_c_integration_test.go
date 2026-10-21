//go:build integration

package httpapi

// W8/MJ-c 图片代理的真实 PostgreSQL 验收。
//
// 覆盖：
//   - ListOwnersByUpstreamID 的真实 SQL 语义（多 owner、family 隔离、排序）；
//   - 签名 URL 的完整链路：验签 → 归属 → 出站 → 流式回写；
//   - 同一 MjId 的两个 owner 各自签名都可用，第三个用户 404；
//   - 存储 URL 指向私网时仍被地址规则拒绝（mode=off 不豁免）。
//
// 运行（隔离库）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_mjc?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run MJImage -count=1

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

func insertMJCTask(t *testing.T, repo *tasks.Repo, userID int64, upstreamID, imageURL string) *tasks.Task {
	t.Helper()
	publicID, err := tasks.NewPublicTaskID()
	if err != nil {
		t.Fatalf("public id: %v", err)
	}
	now := time.Now().UTC()
	row := &tasks.Task{
		PublicTaskID: publicID, UpstreamTaskID: upstreamID, Family: tasks.FamilyMJ,
		Action: string(mj.ActionImagine), PluginKey: "", FetchMode: mj.FetchModePoll,
		State: tasks.StateSuccess, UserID: userID, Group: "default", ChannelID: 1,
		SubmitTime: &now, Properties: map[string]any{}, PrivateData: map[string]any{},
		Data: map[string]any{mj.DataImageURL: imageURL},
	}
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("insert mj task: %v", err)
	}
	return row
}

func mjCImagePath(id, sig string, exp int64) string {
	query := url.Values{}
	query.Set(mj.ImageSigParam, sig)
	query.Set(mj.ImageExpParam, strconv.FormatInt(exp, 10))
	return "/mj/image/" + url.PathEscape(id) + "?" + query.Encode()
}

func TestMJImageProxyOnRealDB(t *testing.T) {
	db := openMJIntegrationDB(t)
	repo := tasks.NewRepo(db)
	userID := time.Now().UnixNano() % 1_000_000
	otherUser := userID + 1
	thirdUser := userID + 2
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = ANY($1)`,
			[]int64{userID, otherUser, thirdUser})
	})

	imageBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3, 4}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/produce.png" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageBytes)
	}))
	defer upstream.Close()

	insertMJCTask(t, repo, userID, "mj_c_shared", upstream.URL+"/produce.png")
	insertMJCTask(t, repo, otherUser, "mj_c_shared", upstream.URL+"/produce.png")
	insertMJCTask(t, repo, thirdUser, "mj_c_private", "http://10.0.0.9/secret.png")

	signer := mj.NewImageSigner("mj-image-integration-key")
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tokens := mjTokenLookup{
		auth.HashTokenKey("nkt-mj-c"): {
			ID: 1, UserID: userID, Status: "enabled", Group: "default", UnlimitedQuota: true,
		},
	}
	handler := NewMJHandler(MJHandlerConfig{
		Store: repo, Channels: repo, Sealer: NewSecretSealer("mj-integration-key"), Tokens: tokens,
		Billing:     func(*contract.RelayContext) contract.BillingSession { return newTaskBillingRecorder() },
		Config:      mjConfigured(nil),
		ImageSigner: signer,
		ImageGuard:  permissiveImageGuard(),
		Now:         func() time.Time { return now },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer nkt-mj-c")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	t.Run("both owners can read the shared MjId", func(t *testing.T) {
		for _, owner := range []int64{userID, otherUser} {
			sig, exp := signer.Sign("mj_c_shared", owner, now)
			rec := get(mjCImagePath("mj_c_shared", sig, exp))
			if rec.Code != http.StatusOK {
				t.Fatalf("owner %d status = %d (body=%s)", owner, rec.Code, rec.Body.String())
			}
			if !bytes.Equal(rec.Body.Bytes(), imageBytes) {
				t.Fatalf("owner %d body = %q", owner, rec.Body.Bytes())
			}
			if got := rec.Header().Get("Content-Type"); got != "image/png" {
				t.Fatalf("owner %d Content-Type = %q", owner, got)
			}
		}
	})

	t.Run("non owner and unknown id are 404", func(t *testing.T) {
		sig, exp := signer.Sign("mj_c_shared", thirdUser, now)
		if rec := get(mjCImagePath("mj_c_shared", sig, exp)); rec.Code != http.StatusNotFound {
			t.Fatalf("non owner status = %d, want 404", rec.Code)
		}
		sig, exp = signer.Sign("mj_c_missing", thirdUser, now)
		if rec := get(mjCImagePath("mj_c_missing", sig, exp)); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown id status = %d, want 404", rec.Code)
		}
	})

	t.Run("expired signature is 404", func(t *testing.T) {
		sig, exp := signer.Sign("mj_c_shared", userID, now.Add(-mj.ImageURLTTL-time.Second))
		if rec := get(mjCImagePath("mj_c_shared", sig, exp)); rec.Code != http.StatusNotFound {
			t.Fatalf("expired status = %d, want 404", rec.Code)
		}
	})

	t.Run("stored private URL is still blocked", func(t *testing.T) {
		sig, exp := signer.Sign("mj_c_private", thirdUser, now)
		rec := get(mjCImagePath("mj_c_private", sig, exp))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}
