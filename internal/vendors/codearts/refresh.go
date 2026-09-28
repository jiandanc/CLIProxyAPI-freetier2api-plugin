package codearts

// Token 续期：使用 refresh_token 与 DPoP 签名换取新 STS 凭证。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/client.go 与 dpop.go

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
	"time"

	"freetier2api-plugin/internal/httpx"
)

// RefreshCredential 执行 Token 续期。
func RefreshCredential(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if cred == nil || cred.RefreshToken == "" {
		return cred, false, nil
	}

	tokenURL := SnapManagerHost + EpOAuthTokens
	proof, errProof := buildDPoPProof(cred.DPoPPrivateKey, tokenURL)
	if errProof != nil {
		return nil, false, fmt.Errorf("build dpop proof: %w", errProof)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", firstNonEmpty(cred.ClientID, ClientID))
	form.Set("refresh_token", cred.RefreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, false, errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("refresh token request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, false, Classify(resp.StatusCode, string(body))
	}

	var res struct {
		RefreshToken string `json:"refresh_token"`
		Credentials  struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, false, fmt.Errorf("decode refresh response: %w", err)
	}

	if res.Credentials.AccessKeyID == "" || res.Credentials.SecretAccessKey == "" {
		return nil, false, fmt.Errorf("refresh token response missing ak/sk")
	}

	updated := *cred
	updated.AccessKeyID = res.Credentials.AccessKeyID
	updated.SecretAccessKey = res.Credentials.SecretAccessKey
	updated.SecurityToken = res.Credentials.SecurityToken
	if res.RefreshToken != "" {
		updated.RefreshToken = res.RefreshToken
	}
	updated.Expiration = res.Credentials.Expiration

	return &updated, true, nil
}

// MergeStorageJSON 合并续期凭证。
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if len(original) == 0 {
		return updated.StorageJSON()
	}

	var m map[string]any
	if err := json.Unmarshal(original, &m); err != nil {
		return updated.StorageJSON()
	}

	m["access_key_id"] = updated.AccessKeyID
	m["secret_access_key"] = updated.SecretAccessKey
	m["security_token"] = updated.SecurityToken
	if updated.RefreshToken != "" {
		m["refresh_token"] = updated.RefreshToken
	}
	if updated.Expiration != "" {
		m["expiration"] = updated.Expiration
	}

	return json.MarshalIndent(m, "", "  ")
}

func buildDPoPProof(jwk map[string]string, htu string) (string, error) {
	if jwk == nil || jwk["d"] == "" || jwk["x"] == "" || jwk["y"] == "" {
		return "", fmt.Errorf("missing dpop private key material")
	}

	xRaw, errX := base64.RawURLEncoding.DecodeString(jwk["x"])
	yRaw, errY := base64.RawURLEncoding.DecodeString(jwk["y"])
	dRaw, errD := base64.RawURLEncoding.DecodeString(jwk["d"])
	if errX != nil || errY != nil || errD != nil {
		return "", fmt.Errorf("decode dpop jwk base64 failed")
	}

	curve := elliptic.P256()
	privKey := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xRaw),
			Y:     new(big.Int).SetBytes(yRaw),
		},
		D: new(big.Int).SetBytes(dRaw),
	}

	header := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   jwk["x"],
			"y":   jwk["y"],
		},
	}
	headerJSON, _ := json.Marshal(header)

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	jti := hex.EncodeToString(b)

	payload := map[string]any{
		"htm": "POST",
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": jti,
	}
	payloadJSON, _ := json.Marshal(payload)

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))

	r, s, errSign := ecdsa.Sign(rand.Reader, privKey, digest[:])
	if errSign != nil {
		return "", errSign
	}

	// 归一化为低 S
	n := curve.Params().N
	halfN := new(big.Int).Rsh(n, 1)
	if s.Cmp(halfN) > 0 {
		s.Sub(n, s)
	}

	pad := func(val *big.Int) []byte {
		buf := make([]byte, 32)
		raw := val.Bytes()
		copy(buf[32-len(raw):], raw)
		return buf
	}

	sigBytes := append(pad(r), pad(s)...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sigBytes), nil
}
