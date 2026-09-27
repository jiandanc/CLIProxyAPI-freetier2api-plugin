package workbuddy

// 本文件实现成长任务的列表 / 报名 / 领奖接口。
//
// 三条路径的形态差异是这里最容易踩的坑：
//   - 列表与报名在对话域（chatBase），带业务信封；
//   - **领奖在 Web 域**（webBase），task_code 在**路径**里、无 body、带 x-client-platform: web。
//     对话域的 /tasks/reward/claim 路径**不存在**，恒返回 400 "task not completed"，
//     这是早期领奖全部失败的真实原因。
//   - 小程序口径的任务码要走带 X-Client-Platform: miniprogram 的同名接口。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Task 是一条成长任务的状态。
type Task struct {
	// Code 是任务标识。
	Code string
	// Title 是任务名。
	Title string
	// Current / Target 是进度。
	Current int64
	Target  int64
	// Claimed 表示奖励已领取。
	Claimed bool
	// Locked 表示任务处于锁定链上（如每日零点解锁的连续任务）。
	Locked bool
	// AcceptStatus 是报名状态：not_accepted / accepted / claimed。
	AcceptStatus string
}

// Claimable 报告任务是否已达标且未领奖。
//
// 本地推算而非依赖上游字段：上游的 claimable 标记并不总是可信
// （部分形态不下发该字段）。
func (t Task) Claimable() bool {
	if t.Claimed || t.Locked {
		return false
	}
	return t.Target > 0 && t.Current >= t.Target
}

// Done 报告任务是否已完成（无论是否领奖）。
func (t Task) Done() bool { return t.Target > 0 && t.Current >= t.Target }

// growthTaskEntry 是任务条目的原始形态。
//
// progress 字段有两种形态：对象 {current,target} 与平铺的 current/target，
// 两种都要兼容（上游不同接口形态不一致）。
type growthTaskEntry struct {
	TaskCode     string          `json:"task_code"`
	TaskCodeAlt  string          `json:"taskCode"`
	Title        string          `json:"title"`
	TitleAlt     string          `json:"task_name"`
	Claimed      bool            `json:"claimed"`
	Locked       bool            `json:"locked"`
	AcceptStatus string          `json:"accept_status"`
	Progress     json.RawMessage `json:"progress"`
	Current      int64           `json:"current"`
	Target       int64           `json:"target"`
}

// toTask 把原始条目归一化。
func (e growthTaskEntry) toTask() Task {
	task := Task{
		Code:         firstNonEmpty(e.TaskCode, e.TaskCodeAlt),
		Title:        firstNonEmpty(e.Title, e.TitleAlt),
		Claimed:      e.Claimed,
		Locked:       e.Locked,
		AcceptStatus: strings.TrimSpace(e.AcceptStatus),
		Current:      e.Current,
		Target:       e.Target,
	}
	// progress 为对象形态时以它为准。
	if len(e.Progress) > 0 {
		var nested struct {
			Current int64 `json:"current"`
			Target  int64 `json:"target"`
		}
		if errUnmarshal := json.Unmarshal(e.Progress, &nested); errUnmarshal == nil {
			if nested.Current != 0 || nested.Target != 0 {
				task.Current = nested.Current
				task.Target = nested.Target
			}
		}
	}
	return task
}

// ListTasks 拉取成长任务列表（默认口径）。
func (c *Client) ListTasks(ctx context.Context, cred *Credential) ([]Task, error) {
	return c.listTasks(ctx, cred, false)
}

// ListTasksMP 拉取小程序口径的任务列表。
//
// 小程序列表是默认列表的**超集**（含 mp 专属任务），合并时必须按任务码去重。
func (c *Client) ListTasksMP(ctx context.Context, cred *Credential) ([]Task, error) {
	return c.listTasks(ctx, cred, true)
}

func (c *Client) listTasks(ctx context.Context, cred *Credential, mp bool) ([]Task, error) {
	if cred.Realm().IsGlobal() {
		// global 账号没有成长任务体系。
		return nil, nil
	}
	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoints.ChatBase+growthTasksPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build tasks request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	if mp {
		applyMPHeaders(req)
	}

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return nil, errCall
	}
	return parseGrowthTasks(data)
}

// parseGrowthTasks 解析任务列表响应。
//
// data 可能是数组，也可能是带 tasks 键的对象。
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var entries []growthTaskEntry
	if errUnmarshal := json.Unmarshal(data, &entries); errUnmarshal != nil {
		var wrapper struct {
			Tasks []growthTaskEntry `json:"tasks"`
		}
		if errWrapper := json.Unmarshal(data, &wrapper); errWrapper != nil {
			return nil, fmt.Errorf("decode tasks: %w", errUnmarshal)
		}
		entries = wrapper.Tasks
	}
	out := make([]Task, 0, len(entries))
	for _, entry := range entries {
		task := entry.toTask()
		if task.Code == "" {
			continue
		}
		out = append(out, task)
	}
	return out, nil
}

// AcceptTasks 批量报名任务。
//
// 报名本身**不产生进度**（进度靠行为事件点亮），它只是让状态机从
// not_accepted 走到 accepted。但少了它，部分任务的事件不会被计分。
func (c *Client) AcceptTasks(ctx context.Context, cred *Credential, codes []string) error {
	return c.acceptTasks(ctx, cred, codes, false)
}

// AcceptTasksMP 用小程序口径批量报名。
func (c *Client) AcceptTasksMP(ctx context.Context, cred *Credential, codes []string) error {
	return c.acceptTasks(ctx, cred, codes, true)
}

func (c *Client) acceptTasks(ctx context.Context, cred *Credential, codes []string, mp bool) error {
	if len(codes) == 0 {
		return nil
	}
	body := map[string]any{"task_codes": codes}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return fmt.Errorf("encode accept request: %w", errEncode)
	}
	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+growthTasksAcceptPath, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build accept request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	if mp {
		applyMPHeaders(req)
	}
	_, errCall := c.doJSON(req)
	return errCall
}

// ClaimReward 领取任务奖励（Web 域）。
//
// 已领取时返回 (0, 0, nil)：上游用 already_claimed 标记表达幂等，
// 把它当错误会让自动任务每次重复执行都报错。
func (c *Client) ClaimReward(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	if strings.TrimSpace(code) == "" {
		return 0, 0, fmt.Errorf("task code is required")
	}
	endpoints := GetEndpoints(RegionCN)
	claimURL := endpoints.WebBase + growthTasksClaimBase + "/" + url.PathEscape(strings.TrimSpace(code)) + "/claim"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if errReq != nil {
		return 0, 0, fmt.Errorf("build claim request: %w", errReq)
	}
	// 领奖走 web 指纹：Referer 指向成长中心页。
	applyWebHeaders(req, cred, endpoints.WebBase+"/profile/growth-center")

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return 0, 0, errCall
	}
	return parseClaimResult(data)
}

// ClaimRewardMP 用小程序口径领奖。
//
// 部分任务/租户在对话域返回 400，此时降级到 Web 域重试。
func (c *Client) ClaimRewardMP(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	if strings.TrimSpace(code) == "" {
		return 0, 0, fmt.Errorf("task code is required")
	}
	endpoints := GetEndpoints(cred.Realm())
	claimURL := endpoints.ChatBase + growthTasksClaimBase + "/" + url.PathEscape(strings.TrimSpace(code)) + "/claim"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if errReq != nil {
		return 0, 0, fmt.Errorf("build mp claim request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	req.Header.Set("X-Client-Platform", "miniprogram")

	data, errCall := c.doJSON(req)
	if errCall != nil {
		// 对话域失败时降级到 Web 域（上游两个口径的可用性不一致）。
		return c.ClaimReward(ctx, cred, code)
	}
	return parseClaimResult(data)
}

// parseClaimResult 解析领奖响应。
func parseClaimResult(data json.RawMessage) (credit, energy int64, err error) {
	var payload struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		// 结构不认识但请求成功：副作用已发生，按无新增处理。
		return 0, 0, nil
	}
	if payload.AlreadyClaimed {
		return 0, 0, nil
	}
	return payload.Credit, payload.Energy, nil
}

// IsMPTaskCode 报告任务码是否属于小程序口径。
//
// 小程序专属任务不会出现在默认列表里，必须走 mp 口径查询与领奖。
func IsMPTaskCode(code string) bool {
	return mpTaskCodes[strings.TrimSpace(code)]
}

// mpTaskCodes 是小程序口径专属任务码。
//
// 说明：Tasks_4..7 处于「每日零点解锁一环」的锁定链上，
// 其判据为预置推测（解锁后需实测校正）；locked 状态的任务永不入队
// （locked 时报名不落账）。
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}
