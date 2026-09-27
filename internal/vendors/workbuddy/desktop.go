package workbuddy

// 本文件实现需要真实对话的辅助方法。
//
// 部分任务（专家使用、技能尝鲜、夜猫子、桌面开学季）的判据要求
// **真实对话产生的服务端 requestId** —— 自造 UUID 不会被计分。
// 因此这些方法会发一次真实对话，并从 SSE 流里抓出上游返回的 id。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// serverRequestIDRe 校验上游返回的 id 形态（32 位 hex，可带 cmb- 前缀）。
var serverRequestIDRe = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// desktopChatSystemPrompt 是辅助对话使用的 system 提示词。
const desktopChatSystemPrompt = "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"

// desktopChatUserPrompt 是辅助对话的用户消息（极短，省额度）。
const desktopChatUserPrompt = "1+1等于几？直接回答。"

// DesktopChat 发一次真实对话但不关心 requestId（夜猫子等场景）。
func (c *Client) DesktopChat(ctx context.Context, cred *Credential, expertID, modelID, modelName, conversationID string) error {
	_, errChat := c.desktopChatInternal(ctx, cred, expertID, modelID, modelName, conversationID, false)
	return errChat
}

// DesktopChatWithRequestID 发一次真实对话并返回上游的服务端 requestId。
//
// 用于需要「真实对话回执」的任务（专家使用、技能尝鲜、桌面开学季）。
func (c *Client) DesktopChatWithRequestID(ctx context.Context, cred *Credential, expertID, modelID, conversationID string) (string, error) {
	return c.desktopChatInternal(ctx, cred, expertID, modelID, modelID, conversationID, true)
}

// desktopChatInternal 是辅助对话的实现。
//
// needRequestID 为 true 时，若流里抓不到合法的服务端 id 会返回错误——
// 因为自造 id 不会被计分，静默继续只会让任务白跑。
func (c *Client) desktopChatInternal(ctx context.Context, cred *Credential, expertID, modelID, modelName, conversationID string, needRequestID bool) (string, error) {
	if strings.TrimSpace(modelID) == "" {
		modelID = "fast-model"
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = modelID
	}
	body := map[string]any{
		"model": modelID,
		"messages": []any{
			map[string]any{"role": "system", "content": desktopChatSystemPrompt},
			map[string]any{"role": "user", "content": desktopChatUserPrompt},
		},
		"agent":       "cli",
		"temperature": 1,
		"stream":      true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return "", fmt.Errorf("encode desktop chat: %w", errEncode)
	}

	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+chatCompletionsPath, bytes.NewReader(encoded))
	if errReq != nil {
		return "", fmt.Errorf("build desktop chat request: %w", errReq)
	}
	c.applyDesktopChatHeaders(req, cred, expertID, conversationID, modelID)

	resp, errDo := c.chatClient(chatHeaderTimeout).Do(req)
	if errDo != nil {
		return "", transientError(errDo)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
		return "", enrichClassifyError(resp.StatusCode, string(raw), resp.Header)
	}

	requestID, errRead := scanServerRequestID(resp.Body)
	if errRead != nil && needRequestID {
		return "", errRead
	}
	return requestID, nil
}

// applyDesktopChatHeaders 写入桌面辅助对话的请求头。
//
// 与常规对话的差别：带 X-Agent-Intent/Type 与桌面 UA，
// 这样上游把它归到「桌面客户端」的行为统计里（部分任务按此判定）。
func (c *Client) applyDesktopChatHeaders(req *http.Request, cred *Credential, expertID, conversationID, modelID string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", desktopUA())
	req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(cred.Realm()).ChatBase, "/"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", DesktopClientVersion)
	if strings.TrimSpace(expertID) != "" {
		req.Header.Set("X-Expert-Id", strings.TrimSpace(expertID))
	}
	if strings.TrimSpace(conversationID) != "" {
		req.Header.Set("X-Conversation-ID", strings.TrimSpace(conversationID))
	}
	req.Header.Set("X-Request-ID", deriveID(cred.UIDValue(), "req")+newHexID()[:6])
}

// scanServerRequestID 从 SSE 流里抓出上游的服务端 requestId。
//
// 判据：第一个形如 "id":"<32hex>"（可带 cmb- 前缀）的值。
// 抓不到返回错误——调用方需要它来构造被计分的事件链。
func scanServerRequestID(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 8*1024), 1<<20)
	deadline := time.Now().Add(30 * time.Second)

	for scanner.Scan() {
		if time.Now().After(deadline) {
			break
		}
		line := scanner.Text()
		index := strings.Index(line, `"id":"`)
		if index < 0 {
			continue
		}
		rest := line[index+len(`"id":"`):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			continue
		}
		candidate := rest[:end]
		if serverRequestIDRe.MatchString(candidate) {
			// 上游可能返回 cmb- 前缀形态，事件链里用去掉前缀的纯 hex。
			return strings.TrimPrefix(candidate, "cmb-"), nil
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return "", fmt.Errorf("read desktop chat stream: %w", errScan)
	}
	return "", fmt.Errorf("no server request id found in chat stream (self-generated ids are not scored)")
}

// DesktopAppearanceSet 设置主题外观。
//
// 注意：**单发 set 不计分**，必须叠加 appearance_skin_apply 事件（见任务动作表）。
func (c *Client) DesktopAppearanceSet(ctx context.Context, cred *Credential, resourceKey string) error {
	endpoints := GetEndpoints(cred.Realm())
	body := map[string]any{"kind": "theme", "resource_key": strings.TrimSpace(resourceKey)}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return fmt.Errorf("encode appearance request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+appearanceSetPath, bytes.NewReader(encoded))
	if errReq != nil {
		return fmt.Errorf("build appearance request: %w", errReq)
	}
	c.applyDesktopHeaders(req, cred)
	_, errCall := c.doJSON(req)
	return errCall
}

// MarketExpert 是专家市场里的一位专家。
type MarketExpert struct {
	ID         string
	Name       string
	Profession string
	Type       string
	Version    string
	Category   string
}

// MarketExpertList 拉取专家市场列表。
//
// expertType 为 "agent" 取单专家、"team" 取专家团。
// **expert_id 必须是市场里的真实值**：自造 id 不会计入任务进度。
func (c *Client) MarketExpertList(ctx context.Context, cred *Credential, expertType string) ([]MarketExpert, error) {
	endpoints := GetEndpoints(cred.Realm())
	body := map[string]any{
		"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc",
	}
	if trimmed := strings.TrimSpace(expertType); trimmed != "" {
		body["expert_type"] = trimmed
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, fmt.Errorf("encode expert list request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+expertMarketListPath, bytes.NewReader(encoded))
	if errReq != nil {
		return nil, fmt.Errorf("build expert list request: %w", errReq)
	}
	c.applyDesktopHeaders(req, cred)

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Experts []struct {
			ExpertID      string   `json:"expert_id"`
			ExpertType    string   `json:"expert_type"`
			DisplayNameZH string   `json:"display_name_zh"`
			ProfessionZH  string   `json:"profession_zh"`
			Version       string   `json:"version"`
			Categories    []string `json:"categories"`
		} `json:"experts"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode expert list: %w", errUnmarshal)
	}
	out := make([]MarketExpert, 0, len(payload.Experts))
	for _, item := range payload.Experts {
		id := strings.TrimSpace(item.ExpertID)
		if id == "" {
			// 空 id 服务端不入账，跳过。
			continue
		}
		expert := MarketExpert{
			ID:         id,
			Name:       strings.TrimSpace(item.DisplayNameZH),
			Profession: strings.TrimSpace(item.ProfessionZH),
			Type:       strings.TrimSpace(item.ExpertType),
			Version:    strings.TrimSpace(item.Version),
		}
		if len(item.Categories) > 0 {
			expert.Category = strings.TrimSpace(item.Categories[0])
		}
		out = append(out, expert)
	}
	return out, nil
}
