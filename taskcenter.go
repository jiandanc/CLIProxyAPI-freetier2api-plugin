package main

// 本文件实现任务中心：全账号扫描、执行队列、单任务/一键完成。
//
// 并发模型（沿用原项目）：
//   - **账号内串行**：同一账号的任务动作必须串行——它们共享账号级的进度状态，
//     并发跑会互相干扰（例如 chat_5 的补报计数）；
//   - **账号间并发**：不同账号互不影响，可并发（1-4）。
//
// 互斥用「每账号一把锁 + 非阻塞 TryLock」：重复点击返回 409，
// 而不是排队等待（排队会让用户以为没反应）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy/tasks"
)

// 队列参数。
const (
	// queueMaxConcurrency 是账号间并发的上限。
	queueMaxConcurrency = 4
	// queueAutoAllTimeout 是「一键完成」的总时长上限。
	//
	// 与请求超时解耦：一键完成要跑 17 个任务、含数次真实对话，
	// 请求超时后任务仍在后台继续（返回 504 告知用户去看队列）。
	queueAutoAllTimeout = 5 * time.Minute
)

// 账号级互斥锁。
//
// 锁条目常驻（账号数有界），避免反复创建 map 条目带来的竞争。
var (
	taskLockMu sync.Mutex
	taskLocks  = map[string]*sync.Mutex{}
)

// tryLockAccount 尝试占用某个账号的任务执行权（非阻塞）。
func tryLockAccount(uid string) bool {
	trimmed := strings.TrimSpace(uid)
	if trimmed == "" {
		return false
	}
	taskLockMu.Lock()
	lock, okLock := taskLocks[trimmed]
	if !okLock {
		lock = &sync.Mutex{}
		taskLocks[trimmed] = lock
	}
	taskLockMu.Unlock()
	return lock.TryLock()
}

// unlockAccount 释放账号的任务执行权。
func unlockAccount(uid string) {
	trimmed := strings.TrimSpace(uid)
	if trimmed == "" {
		return
	}
	taskLockMu.Lock()
	lock, okLock := taskLocks[trimmed]
	taskLockMu.Unlock()
	if okLock {
		lock.Unlock()
	}
}

// taskItem 是扫描结果里的一条待办。
type taskItem struct {
	// Kind 是任务族：growth（成长任务）或 school（开学季）。
	Kind string `json:"kind"`
	// Code 是任务码。
	Code string `json:"code"`
	// Title 是任务名。
	Title string `json:"title"`
	// Current / Target 是进度。
	Current int64 `json:"current"`
	Target  int64 `json:"target"`
	// UsesChat 标记该任务是否消耗真实对话额度。
	UsesChat bool `json:"uses_chat"`
}

// accountScan 是单账号的扫描结果。
type accountScan struct {
	UID      string     `json:"uid"`
	Label    string     `json:"label"`
	Realm    string     `json:"realm"`
	Growth   []taskItem `json:"growth"`
	School   []taskItem `json:"school"`
	InPeriod bool       `json:"in_period"`
	Error    string     `json:"error,omitempty"`
}

// handleTasksScan 扫描全部账号的待办任务。
func handleTasksScan(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	scans := scanAllAccounts(ctx, callbackID)
	total := 0
	for _, scan := range scans {
		total += len(scan.Growth) + len(scan.School)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":            true,
		"accounts":      scans,
		"pending_count": total,
	})
}

// scanAllAccounts 并发扫描每个账号的待办任务。
func scanAllAccounts(ctx context.Context, callbackID string) []accountScan {
	accounts := collectAccounts(ctx, callbackID)
	scans := make([]accountScan, len(accounts))
	var waitGroup sync.WaitGroup
	for index := range accounts {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			scans[slot] = scanOneAccount(ctx, accounts[slot])
		}(index)
	}
	waitGroup.Wait()

	// 按昵称排序，输出稳定。
	sort.Slice(scans, func(i, j int) bool { return scans[i].Label < scans[j].Label })
	return scans
}

// scanOneAccount 扫描单个账号。
func scanOneAccount(ctx context.Context, account *accountContext) accountScan {
	scan := accountScan{
		UID:   account.uid(),
		Label: account.label(),
		Realm: string(account.credential.Realm()),
	}
	if account.isGlobal() {
		// 国际版没有成长任务体系：不报错、不发上游请求。
		return scan
	}

	client := newUpstreamClient(ctx)
	growth, errGrowth := client.ListTasks(ctx, account.credential)
	if errGrowth != nil {
		scan.Error = errGrowth.Error()
		return scan
	}
	// 小程序口径是默认列表的超集，合并时按任务码去重。
	if mpTasks, errMP := client.ListTasksMP(ctx, account.credential); errMP == nil {
		growth = mergeTasks(growth, mpTasks)
	}
	for _, task := range growth {
		if item, okItem := pendingGrowthItem(task); okItem {
			scan.Growth = append(scan.Growth, item)
		}
	}
	// 按动作表顺序排序（顺序即依赖序）。
	sort.SliceStable(scan.Growth, func(i, j int) bool {
		return autoActionIndex(scan.Growth[i].Code) < autoActionIndex(scan.Growth[j].Code)
	})

	// 开学季状态。
	school, errSchool := client.SchoolTasks(ctx, account.credential)
	if errSchool == nil && school != nil {
		scan.InPeriod = school.InPeriod
		for _, task := range school.Tasks {
			if item, okItem := pendingSchoolItem(task); okItem {
				scan.School = append(scan.School, item)
			}
		}
	}
	return scan
}

// pendingGrowthItem 判断成长任务是否应该入队。
//
// 过滤规则（沿用原项目）：
//   - 已领奖 → 跳过；
//   - locked → 跳过（锁定链上的任务报名不落账，只会白跑）；
//   - 无对应动作 → 跳过（需客户端内交互的任务）；
//   - 已达标未领奖 → **入队**（队列会自动领奖）。
func pendingGrowthItem(task workbuddy.Task) (taskItem, bool) {
	if task.Claimed || task.Locked {
		return taskItem{}, false
	}
	action := autoActionFor(task.Code)
	if action == nil {
		return taskItem{}, false
	}
	return taskItem{
		Kind:     "growth",
		Code:     task.Code,
		Title:    firstNonEmptyString(task.Title, action.Desc, task.Code),
		Current:  task.Current,
		Target:   task.Target,
		UsesChat: action.UsesChat,
	}, true
}

// pendingSchoolItem 判断开学季任务是否应该入队。
func pendingSchoolItem(task workbuddy.SchoolTask) (taskItem, bool) {
	switch task.Code {
	case "task_student_verify":
		// 需微信真实学生认证，无法自动化。
		return taskItem{}, false
	case "desktop_chat_1_time":
		if task.Status == "claimed" {
			return taskItem{}, false
		}
	default:
		if task.Status == "claimed" || task.Status == "completed" {
			return taskItem{}, false
		}
	}
	return taskItem{
		Kind:     "school",
		Code:     task.Code,
		Title:    schoolTaskTitle(task.Code),
		Current:  task.Progress,
		Target:   task.TargetCount,
		UsesChat: task.Code == "desktop_chat_1_time",
	}, true
}

// schoolTaskTitle 返回开学季任务的中文名。
func schoolTaskTitle(code string) string {
	switch code {
	case "share_invite":
		return "分享活动"
	case "desktop_chat_1_time":
		return "桌面端体验"
	case "chat_3_times":
		return "和 AI 对话 3 次"
	case "expert_use":
		return "召唤开学季专家"
	}
	return code
}

// mergeTasks 按任务码合并两个任务列表（后者只补缺失）。
func mergeTasks(primary, extra []workbuddy.Task) []workbuddy.Task {
	seen := make(map[string]bool, len(primary))
	out := make([]workbuddy.Task, 0, len(primary)+len(extra))
	for _, task := range primary {
		seen[task.Code] = true
		out = append(out, task)
	}
	for _, task := range extra {
		if seen[task.Code] {
			continue
		}
		out = append(out, task)
	}
	return out
}

// 执行队列。

// queueItem 是队列里的一条执行项。
type queueItem struct {
	UID      string `json:"uid"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Code     string `json:"code"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Credit   int64  `json:"credit,omitempty"`
	Energy   int64  `json:"energy,omitempty"`
	Progress string `json:"progress,omitempty"`
}

// queueState 是队列的全局状态。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	seq       int64
	conc      int
	items     []queueItem
}

var currentQueue = &queueState{}

// taskQueueSnapshot 返回队列快照（供前端轮询）。
//
// 带 Seq 是为了让前端只渲染「自己启动的那一轮」：
// 上一轮的残留条目不会覆盖新扫描的结果视图。
func taskQueueSnapshot() map[string]any {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	items := make([]queueItem, len(currentQueue.items))
	copy(items, currentQueue.items)

	done := 0
	for _, item := range items {
		switch item.Status {
		case "done", "error", "skipped":
			done++
		}
	}
	snapshot := map[string]any{
		"running": currentQueue.running,
		"seq":     currentQueue.seq,
		"conc":    currentQueue.conc,
		"total":   len(items),
		"done":    done,
		"items":   items,
	}
	if !currentQueue.startedAt.IsZero() {
		snapshot["started_at"] = currentQueue.startedAt.Format(time.RFC3339)
	}
	return snapshot
}

// handleTasksRun 启动任务队列。
type taskRunRequest struct {
	Concurrency int  `json:"concurrency"`
	Growth      bool `json:"growth"`
	School      bool `json:"school"`
}

func handleTasksRun(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body := taskRunRequest{Concurrency: 1, Growth: true, School: true}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	concurrency := body.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > queueMaxConcurrency {
		concurrency = queueMaxConcurrency
	}
	runGrowth := body.Growth
	runSchool := body.School
	if !runGrowth && !runSchool {
		runGrowth, runSchool = true, true
	}

	currentQueue.mu.Lock()
	if currentQueue.running {
		currentQueue.mu.Unlock()
		return jsonResponse(http.StatusConflict, map[string]any{
			"ok": false, "error": "队列正在执行中",
		})
	}
	currentQueue.mu.Unlock()

	ctx, cancel := managementContext(req)
	callbackID := hostCallbackID(req)
	scans := scanAllAccounts(ctx, callbackID)
	cancel()

	items := buildQueueItems(scans, runGrowth, runSchool)
	if len(items) == 0 {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": true, "started": false, "message": "全部账号没有待办任务",
		})
	}

	currentQueue.mu.Lock()
	currentQueue.running = true
	currentQueue.seq++
	currentQueue.conc = concurrency
	currentQueue.startedAt = time.Now()
	currentQueue.items = items
	seq := currentQueue.seq
	currentQueue.mu.Unlock()

	go runQueueItems(callbackID, concurrency, seq)
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "total": len(items), "seq": seq, "concurrency": concurrency,
	})
}

// buildQueueItems 把扫描结果摊平成队列项（账号内按依赖序排列）。
func buildQueueItems(scans []accountScan, runGrowth, runSchool bool) []queueItem {
	items := make([]queueItem, 0, 64)
	for _, scan := range scans {
		if runGrowth {
			for _, task := range scan.Growth {
				items = append(items, queueItem{
					UID: scan.UID, Label: scan.Label, Kind: "growth",
					Code: task.Code, Status: "pending",
					Progress: fmt.Sprintf("%d/%d", task.Current, task.Target),
				})
			}
		}
		if runSchool && len(scan.School) > 0 {
			// 开学季每个账号只入一条（一次闭环跑完全部任务）。
			items = append(items, queueItem{
				UID: scan.UID, Label: scan.Label, Kind: "school",
				Code: "school_daily", Status: "pending",
			})
		}
	}
	return items
}

// runQueueItems 执行队列：账号内串行、账号间并发。
func runQueueItems(callbackID string, concurrency int, seq int64) {
	defer func() {
		currentQueue.mu.Lock()
		if currentQueue.seq == seq {
			currentQueue.running = false
		}
		currentQueue.mu.Unlock()
	}()

	ctx := context.Background()
	accounts := collectAccounts(ctx, callbackID)
	byUID := make(map[string]*accountContext, len(accounts))
	for _, account := range accounts {
		byUID[account.uid()] = account
	}

	// 按账号分组，保留队列里的依赖顺序。
	grouped := map[string][]int{}
	order := make([]string, 0, len(byUID))
	for index, item := range queueSnapshotItems() {
		if _, okGroup := grouped[item.UID]; !okGroup {
			order = append(order, item.UID)
		}
		grouped[item.UID] = append(grouped[item.UID], index)
	}

	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	for _, uid := range order {
		account, okAccount := byUID[uid]
		if !okAccount {
			markQueueItems(grouped[uid], "skipped", "账号已不可用", 0, 0)
			continue
		}
		waitGroup.Add(1)
		go func(target *accountContext, indices []int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if !tryLockAccount(target.uid()) {
				// 与单任务/一键完成共用同一把锁：该账号有别的动作在跑。
				markQueueItems(indices, "skipped", "该账号有其它任务动作在执行", 0, 0)
				return
			}
			defer unlockAccount(target.uid())

			// 队列路径的前置：先批量报名，否则上报的事件不落账。
			if accepted := acceptPendingTasks(ctx, target); accepted > 0 {
				logger.Debug("queue: accepted %d task(s) for %s", accepted, target.label())
				time.Sleep(1050 * time.Millisecond)
			}
			for _, index := range indices {
				item := queueItemAt(index)
				markQueueItem(index, "running", "")
				var message string
				var credit, energy int64
				var errRun error
				switch item.Kind {
				case "growth":
					message, credit, energy, errRun = runGrowthTask(ctx, target, item.Code)
				case "school":
					message, credit, energy, errRun = runSchoolDaily(ctx, target)
				}
				if errRun != nil {
					markQueueItem(index, "error", errRun.Error())
				} else {
					setQueueItemResult(index, "done", message, credit, energy)
				}
				time.Sleep(1050 * time.Millisecond)
			}
		}(account, grouped[uid])
	}
	waitGroup.Wait()
	logger.Info("task queue finished (seq=%d)", seq)
}

// 队列状态访问器（避免在 goroutine 里长时间持锁）。

func queueSnapshotItems() []queueItem {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	items := make([]queueItem, len(currentQueue.items))
	copy(items, currentQueue.items)
	return items
}

func queueItemAt(index int) queueItem {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	if index < 0 || index >= len(currentQueue.items) {
		return queueItem{}
	}
	return currentQueue.items[index]
}

func markQueueItem(index int, status, message string) {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	if index < 0 || index >= len(currentQueue.items) {
		return
	}
	currentQueue.items[index].Status = status
	if message != "" {
		currentQueue.items[index].Message = message
	}
}

func setQueueItemResult(index int, status, message string, credit, energy int64) {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	if index < 0 || index >= len(currentQueue.items) {
		return
	}
	currentQueue.items[index].Status = status
	currentQueue.items[index].Message = message
	currentQueue.items[index].Credit = credit
	currentQueue.items[index].Energy = energy
}

func markQueueItems(indices []int, status, message string, credit, energy int64) {
	for _, index := range indices {
		setQueueItemResult(index, status, message, credit, energy)
	}
}

// acceptPendingTasks 批量报名未接受的任务（队列与一键完成的前置）。
func acceptPendingTasks(ctx context.Context, account *accountContext) int {
	if account.isGlobal() {
		return 0
	}
	client := newUpstreamClient(ctx)
	tasksList, errTasks := client.ListTasks(ctx, account.credential)
	if errTasks != nil {
		return 0
	}
	var codes []string
	for _, task := range tasksList {
		if task.Claimed || task.Locked {
			continue
		}
		status := strings.ToLower(task.AcceptStatus)
		if status == "accepted" || status == "completed" || status == "claimed" {
			continue
		}
		if autoActionFor(task.Code) == nil && !workbuddy.IsMPTaskCode(task.Code) {
			continue
		}
		codes = append(codes, task.Code)
	}
	if len(codes) == 0 {
		return 0
	}
	accepted := 0
	for start := 0; start < len(codes); start += 20 {
		end := start + 20
		if end > len(codes) {
			end = len(codes)
		}
		if errAccept := client.AcceptTasks(ctx, account.credential, codes[start:end]); errAccept == nil {
			accepted += end - start
		}
		time.Sleep(1050 * time.Millisecond)
	}
	return accepted
}

// runGrowthTask 执行单个成长任务（含达标预检、执行与自动领奖）。
func runGrowthTask(ctx context.Context, account *accountContext, code string) (string, int64, int64, error) {
	action := autoActionFor(code)
	if action == nil {
		return "", 0, 0, fmt.Errorf("该任务没有对应的自动动作（可能需要客户端内交互）")
	}
	client := newUpstreamClient(ctx)

	// 达标预检：如果任务已经完成/达标，直接尝试领奖，绝不再重新执行动作（避免重复消耗额度）。
	task, errTask := findGrowthTask(ctx, client, account.credential, code)
	if errTask == nil && task != nil {
		if task.Claimed {
			recordTaskResult(account.uid(), code, "任务已完成且已领奖", "done", 0, 0)
			return "任务已完成且已领奖", 0, 0, nil
		}
		if task.Done() {
			credit, energy := claimGrowthReward(ctx, client, account.credential, code)
			msg := "任务已达标（跳过重复执行）"
			if credit > 0 || energy > 0 {
				msg = fmt.Sprintf("任务已达标，已领奖 +%d 积分 +%d 能量", credit, energy)
			}
			recordTaskResult(account.uid(), code, msg, "done", credit, energy)
			return msg, credit, energy, nil
		}
	}

	message, errRun := action.Run(ctx, client, account.credential)
	if errRun != nil {
		return "", 0, 0, errRun
	}
	recordTaskResult(account.uid(), code, message, "done", 0, 0)

	// 等异步计分落定后自动领奖。
	if waitGrowthClaimable(ctx, client, account.credential, code) {
		credit, energy := claimGrowthReward(ctx, client, account.credential, code)
		if credit > 0 || energy > 0 {
			message += fmt.Sprintf("（领奖 +%d 积分 +%d 能量）", credit, energy)
			recordTaskResult(account.uid(), code, message, "done", credit, energy)
		}
		return message, credit, energy, nil
	}
	return message, 0, 0, nil
}

// waitGrowthClaimable 轮询等待任务变为可领奖。
//
// 上报 200 ≠ 计分：上游计分是异步的（实测 5-8 秒落定），
// 立即回读会误判未达标从而跳过领奖。
func waitGrowthClaimable(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential, code string) bool {
	mp := workbuddy.IsMPTaskCode(code)
	attempts := 4
	if mp {
		attempts = 2
	}
	for attempt := 1; attempt < attempts; attempt++ {
		if !sleepCtx(ctx, 3*time.Second) {
			return false
		}
		task, errTask := findGrowthTask(ctx, client, credential, code)
		if errTask != nil {
			continue
		}
		if task == nil {
			return false
		}
		if task.Claimed {
			return false
		}
		if task.Claimable() || task.Done() {
			return true
		}
	}
	return false
}

// findGrowthTask 按任务码查任务（自动选择默认或小程序口径）。
func findGrowthTask(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential, code string) (*workbuddy.Task, error) {
	var list []workbuddy.Task
	var errList error
	if workbuddy.IsMPTaskCode(code) {
		list, errList = client.ListTasksMP(ctx, credential)
	} else {
		list, errList = client.ListTasks(ctx, credential)
	}
	if errList != nil {
		return nil, errList
	}
	for index := range list {
		if list[index].Code == code {
			return &list[index], nil
		}
	}
	return nil, nil
}

// claimGrowthReward 领取任务奖励。
func claimGrowthReward(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential, code string) (int64, int64) {
	if workbuddy.IsMPTaskCode(code) {
		credit, energy, errClaim := client.ClaimRewardMP(ctx, credential, code)
		if errClaim != nil {
			logger.Debug("claim mp reward for %s failed: %v", code, errClaim)
			return 0, 0
		}
		return credit, energy
	}
	credit, energy, errClaim := client.ClaimReward(ctx, credential, code)
	if errClaim != nil {
		logger.Debug("claim reward for %s failed: %v", code, errClaim)
		return 0, 0
	}
	return credit, energy
}

// runSchoolDaily 执行单账号的开学季闭环。
func runSchoolDaily(ctx context.Context, account *accountContext) (string, int64, int64, error) {
	client := newUpstreamClient(ctx)
	message, errRun := tasks.RunSchool(ctx, client, account.credential)
	if errRun != nil {
		return "", 0, 0, errRun
	}
	if message == "" {
		return "开学季无需动作（活动期外或已完成）", 0, 0, nil
	}
	return message, 0, 0, nil
}

// recordTaskResult 记录任务执行结果到状态文件。
func recordTaskResult(uid, code, message, status string, credit, energy int64) {
	if strings.TrimSpace(uid) == "" {
		return
	}
	key := taskStateKey(uid, code)
	mutateState(func(state *pluginState) {
		state.Tasks[key] = taskRecord{
			LastRunAt:   nowRFC3339(),
			LastStatus:  status,
			LastMessage: message,
			Credit:      credit,
			Energy:      energy,
		}
	})
}

// handleTasksAuto 对单个账号执行单个任务或一键完成。
type taskAutoRequest struct {
	AuthID    string `json:"auth_id"`
	UID       string `json:"uid"`
	AccountID string `json:"account_id"`
	TaskCode  string `json:"task_code"`
}

func handleTasksAuto(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body taskAutoRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authID := firstNonEmptyString(body.AuthID, body.UID, body.AccountID)
	code := strings.TrimSpace(body.TaskCode)

	ctx, cancel := managementContext(req)
	defer cancel()
	account := findAccount(ctx, hostCallbackID(req), authID)
	if account == nil {
		return jsonResponse(http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"})
	}
	if !tryLockAccount(account.uid()) {
		return jsonResponse(http.StatusConflict, map[string]any{
			"ok": false, "error": "该账号有任务动作正在执行中",
		})
	}
	defer unlockAccount(account.uid())

	// 空任务码 = 一键完成全部。
	if code == "" {
		results := runAutoAll(ctx, account)
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
	}

	if autoActionFor(code) == nil {
		return jsonResponse(http.StatusNotImplemented, map[string]any{
			"ok": false, "error": "该任务没有对应的自动动作（可能需要客户端内交互）",
		})
	}
	message, credit, energy, errRun := runGrowthTask(ctx, account, code)
	if errRun != nil {
		recordTaskResult(account.uid(), code, errRun.Error(), "error", 0, 0)
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": false, "error": errRun.Error(), "task_code": code,
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "task_code": code, "message": message,
		"credit": credit, "energy": energy,
	})
}

// handleTasksAutoAll 对全部账号依次执行一键完成。
//
// 与「单账号一键完成」共用 runAutoAll 与账号级互斥锁：正在跑的账号会被跳过，
// 而不是排队等待（排队会让用户以为没反应）。
func handleTasksAutoAll(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	accounts := collectAccounts(ctx, callbackID)
	if len(accounts) == 0 {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": true, "accounts": 0, "message": "没有可用账号",
		})
	}

	type accountResult struct {
		Label   string          `json:"label"`
		UID     string          `json:"uid"`
		Realm   string          `json:"realm"`
		Skipped string          `json:"skipped,omitempty"`
		Results []autoAllResult `json:"results,omitempty"`
	}
	out := make([]accountResult, 0, len(accounts))
	for _, account := range accounts {
		entry := accountResult{
			Label: account.label(),
			UID:   account.uid(),
			Realm: string(account.credential.Realm()),
		}
		if account.isGlobal() {
			// 国际版没有成长任务体系，直接跳过（不发上游请求）。
			entry.Skipped = "国际版账号没有成长任务"
			out = append(out, entry)
			continue
		}
		if !tryLockAccount(account.uid()) {
			entry.Skipped = "该账号有其它任务动作在执行"
			out = append(out, entry)
			continue
		}
		entry.Results = runAutoAll(ctx, account)
		unlockAccount(account.uid())
		out = append(out, entry)
		// 账号之间留出间隔：全账号连续动作容易被上游风控注意到。
		if !sleepCtx(ctx, accountGap) {
			break
		}
	}

	done := 0
	for _, entry := range out {
		for _, result := range entry.Results {
			if result.Status == "done" {
				done++
			}
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "accounts": len(out), "tasks_done": done, "results": out,
	})
}

// autoAllResult 是一键完成里单条任务的结果。
type autoAllResult struct {
	Code     string `json:"task_code"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Credit   int64  `json:"credit,omitempty"`
	Energy   int64  `json:"energy,omitempty"`
	Progress string `json:"progress,omitempty"`
}

// runAutoAll 按动作表顺序执行全部可自动完成的任务。
//
// 顺序即依赖序：first_buddy 需要前置的活跃上报，专家类需要前置的召唤链。
func runAutoAll(ctx context.Context, account *accountContext) []autoAllResult {
	client := newUpstreamClient(ctx)
	results := make([]autoAllResult, 0, len(autoActions))

	// 前置：批量报名（不报名的话部分任务的事件不落账）。
	if accepted := acceptPendingTasks(ctx, account); accepted > 0 {
		results = append(results, autoAllResult{
			Code: "(批量报名)", Status: "done",
			Message: fmt.Sprintf("报名 %d 个任务", accepted),
		})
		time.Sleep(1050 * time.Millisecond)
	}

	for _, action := range autoActions {
		task, errTask := findGrowthTask(ctx, client, account.credential, action.Code)
		if errTask != nil {
			results = append(results, autoAllResult{
				Code: action.Code, Status: "error", Message: errTask.Error(),
			})
			continue
		}
		if task == nil {
			results = append(results, autoAllResult{
				Code: action.Code, Status: "skipped", Message: "该账号无此任务",
			})
			continue
		}
		if task.Claimed {
			results = append(results, autoAllResult{
				Code: action.Code, Status: "skipped", Message: "已领取过奖励",
			})
			continue
		}
		// 窗口约束：夜猫子只在夜间计分。
		if action.Code == "black_cat" && !tasks.InNightWindow(nowFunc()) {
			results = append(results, autoAllResult{
				Code: action.Code, Status: "skipped",
				Message: "当前不在 23:00-08:00 计分窗口，排程会在夜间自动补足",
			})
			continue
		}

		message, credit, energy, errRun := runGrowthTask(ctx, account, action.Code)
		result := autoAllResult{Code: action.Code, Credit: credit, Energy: energy}
		if errRun != nil {
			result.Status = "error"
			result.Message = errRun.Error()
			recordTaskResult(account.uid(), action.Code, result.Message, "error", 0, 0)
		} else {
			result.Status = "done"
			result.Message = message
		}
		// 回读最终进度供展示。
		if latest, errLatest := findGrowthTask(ctx, client, account.credential, action.Code); errLatest == nil && latest != nil {
			result.Progress = fmt.Sprintf("%d/%d", latest.Current, latest.Target)
		}
		results = append(results, result)
		time.Sleep(1050 * time.Millisecond)
	}
	return results
}

// collectAccounts 收集本插件名下的可用账号。
func collectAccounts(ctx context.Context, callbackID string) []*accountContext {
	accounts := make([]*accountContext, 0, 8)
	forEachAccount(ctx, callbackID, func(_ context.Context, account *accountContext) error {
		accounts = append(accounts, account)
		return nil
	})
	return accounts
}

// findAccount 按标识查找账号（支持 auth_id / uid / auth_index）。
func findAccount(ctx context.Context, callbackID, identifier string) *accountContext {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return nil
	}
	var found *accountContext
	forEachAccount(ctx, callbackID, func(_ context.Context, account *accountContext) error {
		if found != nil {
			return nil
		}
		if account.uid() == trimmed || account.entry.ID == trimmed ||
			account.entry.Name == trimmed || account.entry.AuthIndex == trimmed ||
			account.entry.Label == trimmed {
			found = account
		}
		return nil
	})
	return found
}

// schoolStatusForAccounts 返回开学季活动状态（供管理页展示）。
func schoolStatusForAccounts(ctx context.Context, callbackID string) []map[string]any {
	accounts := collectAccounts(ctx, callbackID)
	out := make([]map[string]any, 0, len(accounts))
	var waitGroup sync.WaitGroup
	var mu sync.Mutex

	for _, account := range accounts {
		waitGroup.Add(1)
		go func(target *accountContext) {
			defer waitGroup.Done()
			entry := map[string]any{
				"uid":   target.uid(),
				"label": target.label(),
				"realm": string(target.credential.Realm()),
			}
			if target.isGlobal() {
				entry["in_period"] = false
				entry["error"] = "global realm（无开学季活动）"
				mu.Lock()
				out = append(out, entry)
				mu.Unlock()
				return
			}
			client := newUpstreamClient(ctx)
			status, errStatus := client.SchoolTasks(ctx, target.credential)
			if errStatus != nil {
				entry["error"] = errStatus.Error()
			} else if status != nil {
				entry["in_period"] = status.InPeriod
				list := make([]map[string]any, 0, len(status.Tasks))
				for _, task := range status.Tasks {
					list = append(list, map[string]any{
						"task_code": task.Code, "title": schoolTaskTitle(task.Code),
						"status": task.Status, "progress": task.Progress, "target": task.TargetCount,
					})
				}
				entry["tasks"] = list
			}
			if chances, errChances := client.SchoolChances(ctx, target.credential); errChances == nil {
				entry["chances"] = chances
			}
			mu.Lock()
			out = append(out, entry)
			mu.Unlock()
		}(account)
	}
	waitGroup.Wait()
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["label"]) < fmt.Sprint(out[j]["label"])
	})
	return out
}
