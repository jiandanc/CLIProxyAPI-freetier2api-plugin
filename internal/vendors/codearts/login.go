package codearts

// 本文件实现 CodeArts 的华为云 OAuth2 (Ticket + PKCE + DPoP) 登录流程。
// 参考：/Users/jiandan/Workspaces/codearts2api/cmd/login/main.go 与 internal/server/oauth.go

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
)

type pendingLogin struct {
	TicketID       string
	Secret         string
	Verifier       string
	DPoPPrivateKey map[string]string
	CreatedAt      time.Time
}

var (
	loginMu    sync.Mutex
	loginStore = map[string]*pendingLogin{}
)

// LoginStart 发起华为云 CodeArts 授权登录。
func LoginStart(ctx context.Context) (*pluginapi.AuthLoginStartResponse, error) {
	ticketID := randomHex(16)
	secret := randomHex(16)
	verifier, challenge := generatePKCE()
	dpopJWK, err := generateDPoPJWK()
	if err != nil {
		return nil, fmt.Errorf("generate dpop jwk: %w", err)
	}

	redirectURI := "http://127.0.0.1:7866/oauth/callback"
	v := url.Values{}
	v.Set("client_id", ClientID)
	v.Set("response_type", "code")
	v.Set("ticket_id", ticketID)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	v.Set("redirect_uri", redirectURI)

	authURL := PortalHost + "/authorize?" + v.Encode()
	sessionID := "codearts_" + randomHex(8)

	loginMu.Lock()
	loginStore[sessionID] = &pendingLogin{
		TicketID:       ticketID,
		Secret:         secret,
		Verifier:       verifier,
		DPoPPrivateKey: dpopJWK,
		CreatedAt:      time.Now(),
	}
	loginMu.Unlock()

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freetier",
		URL:       authURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll 轮询华为云 ticket 登录结果。
func LoginPoll(ctx context.Context, sessionID string) (*pluginapi.AuthLoginPollResponse, error) {
	loginMu.Lock()
	session, ok := loginStore[sessionID]
	loginMu.Unlock()

	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期",
		}, nil
	}

	path := SnapManagerHost + EpLoginTicket + "?ticket_id=" + url.QueryEscape(session.TicketID) + "&secret=" + url.QueryEscape(session.Secret)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("plugin-name", "snap_AIIDE")
	req.Header.Set("plugin-version", "5.2.0")

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("poll login ticket failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户在华为云网页端完成授权...",
		}, nil
	}

	var res struct {
		UserName     string `json:"user_name"`
		UserID       string `json:"user_id"`
		DomainID     string `json:"domain_id"`
		RefreshToken string `json:"refresh_token"`
		Credential   struct {
			Access        string `json:"access"`
			Secret        string `json:"secret"`
			SecurityToken string `json:"security_token"`
			ExpiresAt     string `json:"expires_at"`
		} `json:"credential"`
		Credentials struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode ticket response: %w", err)
	}

	ak := firstNonEmpty(res.Credentials.AccessKeyID, res.Credential.Access)
	sk := firstNonEmpty(res.Credentials.SecretAccessKey, res.Credential.Secret)
	st := firstNonEmpty(res.Credentials.SecurityToken, res.Credential.SecurityToken)

	if ak == "" || sk == "" {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成华为云登录...",
		}, nil
	}

	loginMu.Lock()
	delete(loginStore, sessionID)
	loginMu.Unlock()

	uid := firstNonEmpty(res.UserID, res.UserName, "codearts-user")
	cred := &Credential{
		Vendor:          VendorID,
		UserID:          res.UserID,
		UserName:        res.UserName,
		DomainID:        res.DomainID,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SecurityToken:   st,
		RefreshToken:    res.RefreshToken,
		CodeVerifier:    session.Verifier,
		ClientID:        ClientID,
		DPoPPrivateKey:  session.DPoPPrivateKey,
		Label:           "CodeArts-" + uid,
	}

	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, errStorage
	}

	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "华为云账号登录成功",
		Auth: pluginapi.AuthData{
			Provider:    "freetier",
			FileName:    "codearts-" + uid + ".json",
			Label:       cred.Label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// OwnsLoginSession 报告该会话是否属于 CodeArts。
func OwnsLoginSession(sessionID string) bool {
	norm := strings.ToLower(strings.TrimSpace(sessionID))
	if strings.HasPrefix(norm, "codearts_") {
		return true
	}
	loginMu.Lock()
	defer loginMu.Unlock()
	s, ok := loginStore[sessionID]
	if !ok {
		return false
	}
	return time.Since(s.CreatedAt) <= 15*time.Minute
}

func generatePKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func generateDPoPJWK() (map[string]string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pad := func(b *big.Int, sz int) []byte {
		buf := make([]byte, sz)
		raw := b.Bytes()
		copy(buf[sz-len(raw):], raw)
		return buf
	}
	return map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(pad(key.PublicKey.X, 32)),
		"y":   base64.RawURLEncoding.EncodeToString(pad(key.PublicKey.Y, 32)),
		"d":   base64.RawURLEncoding.EncodeToString(pad(key.D, 32)),
	}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
