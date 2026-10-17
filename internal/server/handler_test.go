package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/upstream"
)

const hardCreditFrame = "event:error\ndata:{\"type\":\"error\",\"error\":{\"type\":\"proxy_error\",\"message\":\"免费额度已用完，请升级套餐\",\"code\":40201}}\n\n"

func testAuth(uid, token string) *auth.Auth {
	return &auth.Auth{
		UID:          uid,
		AccessToken:  token,
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
}

func testHandler(t *testing.T, upstreamFn http.HandlerFunc) (*Handler, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(upstreamFn)
	t.Cleanup(srv.Close)
	t.Setenv("LB2A_UPSTREAM_BASE", srv.URL)

	p := pool.New("")
	p.Add(testAuth("a", "tok-a"))
	p.Add(testAuth("b", "tok-b"))

	return NewHandler(Config{
		Pool:         p,
		Upstream:     upstream.New(),
		HardCooldown: 12 * time.Hour,
		SoftCooldown: time.Minute,
		ErrThreshold: 3,
		ErrCooldown:  10 * time.Minute,
	}), p
}

func writeNormalSSE(w io.Writer, content string) {
	fmt.Fprintf(w, "data:{\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", content)
	io.WriteString(w, "data:[DONE]\n\n")
}

func doChat(h *Handler) *httptest.ResponseRecorder {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

func statusByUID(p *pool.Pool, uid string) pool.Status {
	for _, st := range p.List() {
		if st.UID == uid {
			return st
		}
	}
	return pool.Status{}
}

func responseContent(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices: %s", rec.Body.String())
	}
	return resp.Choices[0].Message.Content
}

// 首个账号额度不足（200 SSE 流首错误帧）时，必须冷却它并换到下一个账号。
func TestChatRotatesOnHardCredit(t *testing.T) {
	h, p := testHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "tok-a":
			io.WriteString(w, hardCreditFrame)
		case "tok-b":
			writeNormalSSE(w, "ok")
		default:
			http.Error(w, "bad token", http.StatusUnauthorized)
		}
	})

	rec := doChat(h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := responseContent(t, rec); got != "ok" {
		t.Fatalf("content=%q, want ok", got)
	}

	a := statusByUID(p, "a")
	if a.Credits != 0 || !a.Cooling {
		t.Fatalf("account a: credits=%d cooling=%v, want 0/true", a.Credits, a.Cooling)
	}
	if b := statusByUID(p, "b"); b.Cooling {
		t.Fatalf("account b should stay healthy: %+v", b)
	}
}

// 错误帧不在流首 4KB 内时，ChatStream 拦不到，非流式聚合阶段必须继续换号，
// 不能把额度不足误报成 502。
func TestChatRotatesOnMidStreamHardCredit(t *testing.T) {
	h, p := testHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "tok-a":
			// 先写 >4KB 正常帧，再写错误帧。
			for i := 0; i < 200; i++ {
				fmt.Fprintf(w, "data:{\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%s\"}}]}\n\n",
					strings.Repeat("y", 30))
			}
			io.WriteString(w, hardCreditFrame)
		case "tok-b":
			writeNormalSSE(w, "ok")
		default:
			http.Error(w, "bad token", http.StatusUnauthorized)
		}
	})

	rec := doChat(h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := responseContent(t, rec); got != "ok" {
		t.Fatalf("content=%q, want ok", got)
	}
	if a := statusByUID(p, "a"); a.Credits != 0 || !a.Cooling {
		t.Fatalf("account a: credits=%d cooling=%v, want 0/true", a.Credits, a.Cooling)
	}
}

// 所有账号都没额度时，必须把每个账号都试一次，然后明确报额度不足。
func TestChatAllAccountsExhausted(t *testing.T) {
	h, p := testHandler(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, hardCreditFrame)
	})

	rec := doChat(h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "out of credits") {
		t.Fatalf("body should mention out of credits: %s", rec.Body.String())
	}
	for _, uid := range []string{"a", "b"} {
		st := statusByUID(p, uid)
		if st.Credits != 0 || !st.Cooling {
			t.Fatalf("account %s: credits=%d cooling=%v, want 0/true", uid, st.Credits, st.Cooling)
		}
	}
}
