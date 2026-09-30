package minimaxcode

import (
	"encoding/json"
	"testing"
)

func TestParseCredential(t *testing.T) {
	raw := []byte(`{
		"access_token": "at_123",
		"refresh_token": "rt_456",
		"expires_at": 1780000000,
		"user_id": "u_999",
		"agent_id": "12345678",
		"email": "test@minimax.io",
		"region": "global"
	}`)

	cred, err := ParseCredential(raw, RegionCN)
	if err != nil {
		t.Fatalf("ParseCredential failed: %v", err)
	}

	if cred.AccessToken != "at_123" {
		t.Errorf("expected access_token 'at_123', got %q", cred.AccessToken)
	}
	if cred.RefreshToken != "rt_456" {
		t.Errorf("expected refresh_token 'rt_456', got %q", cred.RefreshToken)
	}
	if cred.UserID != "u_999" {
		t.Errorf("expected user_id 'u_999', got %q", cred.UserID)
	}
	if cred.AgentID != "12345678" {
		t.Errorf("expected agent_id '12345678', got %q", cred.AgentID)
	}
	if cred.Region != RegionGlobal {
		t.Errorf("expected region 'global', got %q", cred.Region)
	}
	if cred.AuthMode != "oauth" {
		t.Errorf("expected auth_mode 'oauth', got %q", cred.AuthMode)
	}
	if cred.UUID == "" {
		t.Errorf("expected non-empty UUID")
	}
	if len(cred.DeviceID) != 8 {
		t.Errorf("expected 8-digit device ID, got %q", cred.DeviceID)
	}
}

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		fileName string
		target   Region
		want     bool
	}{
		{"minimaxcodecn filename", `{"token":"abc"}`, "minimaxcodecn-u1.json", RegionCN, true},
		{"minimaxcodeglobal filename", `{"token":"abc"}`, "minimaxcodeglobal-u1.json", RegionGlobal, true},
		{"minimaxcn filename", `{"token":"abc"}`, "minimaxcn-u1.json", RegionCN, true},
		{"minimaxglobal filename", `{"token":"abc"}`, "minimaxglobal-u1.json", RegionGlobal, true},
		{"wrong region filename", `{"token":"abc"}`, "minimaxcodecn-u1.json", RegionGlobal, false},
		{"vendor field exact CN", `{"vendor":"minimaxcodecn","token":"abc"}`, "acc.json", RegionCN, true},
		{"vendor field exact Global", `{"vendor":"minimaxcodeglobal","token":"abc"}`, "acc.json", RegionGlobal, true},
		{"other vendor rejected", `{"vendor":"qoder","token":"abc"}`, "acc.json", RegionCN, false},
		{"agent_id content feature", `{"agent_id":"123","token":"abc","region":"cn"}`, "acc.json", RegionCN, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal([]byte(tc.raw), &m)
			got := LooksLikeCredential(m, tc.fileName, "", tc.target)
			if got != tc.want {
				t.Fatalf("LooksLikeCredential() = %v, want %v", got, tc.want)
			}
		})
	}
}
