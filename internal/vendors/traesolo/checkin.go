package traesolo

// 每日签到：TRAE SOLO 签到领取积分与状态查询。
// 参考：/Users/jiandan/Workspaces/trae2api-more/internal/upstream/

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

// SupportsCheckin 报告本供应商支持签到。
func SupportsCheckin() bool {
	return true
}

// CheckinDeviceID 根据用户账号稳定派生 16 位设备 ID。
func CheckinDeviceID(identity string, generation int) string {
	if identity == "" {
		identity = "default_trae_user"
	}
	material := identity
	if generation > 0 {
		material = fmt.Sprintf("%s#gen%d", identity, generation)
	}
	sum := sha256.Sum256([]byte(material))
	n := new(big.Int).SetBytes(sum[:])
	n.Mod(n, big.NewInt(1e16))
	return fmt.Sprintf("%016d", n)
}

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
}

// Checkin 执行一次签到。
func Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential or access token missing")
	}

	identity := cred.UID
	if identity == "" {
		identity = cred.DeviceID
	}
	deviceID := CheckinDeviceID(identity, 0)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, UgHost+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("X-Device-Type", "windows")

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("traesolo checkin request failed: %w", errDo)
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

	// 9074 或特定提示代表今日已签到
	if result.Code == 9074 || result.Code == 1001 {
		return &CheckinResult{
			Already: true,
			Message: firstNonEmpty(result.Msg, "今日已完成签到"),
		}, nil
	}

	return nil, fmt.Errorf("checkin failed: code=%d msg=%s", result.Code, result.Msg)
}
