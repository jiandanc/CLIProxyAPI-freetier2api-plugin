package trae

// 签到相关实现：Trae 国内版支持签到，国际版不支持。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"freetier2api-plugin/internal/httpx"
)

func SupportsCheckin(r Region) bool {
	return r == RegionCN
}

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
}

func Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}
	if cred.Region != RegionCN {
		return nil, fmt.Errorf("trae 国际版无签到活动")
	}

	identity := cred.UID
	if identity == "" {
		identity = cred.DeviceID
	}
	deviceID := deriveCheckinDeviceID(identity)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, HostAuthCN+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("X-Device-Brand", "83DG")
	req.Header.Set("X-Device-Type", "windows")

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("trae checkin request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit int64 `json:"credit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode checkin response: %w", err)
	}

	if result.Code == 0 {
		return &CheckinResult{
			Already: false,
			Credit:  result.Data.Credit,
			Message: "签到成功",
		}, nil
	}

	if result.Code == 9074 || result.Code == 1001 {
		return &CheckinResult{
			Already: true,
			Message: firstNonEmpty(result.Msg, "今日已完成签到"),
		}, nil
	}

	return nil, fmt.Errorf("checkin failed: code=%d msg=%s", result.Code, result.Msg)
}

func deriveCheckinDeviceID(identity string) string {
	if identity == "" {
		identity = "default_trae_user"
	}
	sum := sha256.Sum256([]byte(identity))
	n := new(big.Int).SetBytes(sum[:])
	n.Mod(n, big.NewInt(1e16))
	return fmt.Sprintf("%016d", n)
}
