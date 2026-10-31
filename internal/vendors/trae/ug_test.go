package trae

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 活动福利领取的行为测试。
//
// 这类端点的失败原因全写在 body 的 message 里，状态码经常还是 200：把
// 「已领取」当成「失败」会让人反复重试，把「活动已结束」当成「已领取」又会
// 掩盖真的没领到。所以归类逻辑必须逐个码位固定下来。

// ugTestCredential 造一个国内版凭证（uid 决定派生的设备头）。
func ugTestCredential() *Credential {
	return &Credential{
		Vendor:      VendorIDCN,
		Region:      RegionCN,
		AccessToken: "test-access-token",
		UID:         "2487573687317868",
	}
}

// ugTestConfig 指向假上游。
//
// 插件不发真实请求，一律经宿主 http.do 回调，所以用例必须先 stubHost。
func ugTestConfig(t *testing.T, host string) Config {
	t.Helper()
	return testConfig(t, host)
}

// TestUGActionsAreBatchable 验证：批量入口都是「无需输入、可直接 POST」的。
//
// 兑换码需要用户提供内容，而手动触发路由只传任务族名、没有请求体入口，所以
// 那条链路已删除——留着它只会是一个永远调不到的实现。
//
// cobrand/claim 与两个 share_link 需要活动 ID / 作品 ID（上游实测回
// 4001 / 9080「param is invalid」），插件取不到，同样不进批量入口：每轮白打
// 3 次注定失败的请求，只会拖慢整轮并把真实原因糊成「暂不可领」。
func TestUGActionsAreBatchable(t *testing.T) {
	actions := UGActions()
	if len(actions) != 2 {
		t.Fatalf("活动入口数 = %d, 期望 2（竞赛奖励 + 作品抽奖）", len(actions))
	}
	// 需要入参的端点已从代码里删掉（连常量一起），这里用路径字面量做黑名单，
	// 防止日后有人凭记忆把它们加回批量入口。
	for _, excluded := range []string{
		"/trae/api/v2/ug/cobrand/claim",
		"/trae/api/v2/ug/work_fission/share_link",
		"/trae/api/v2/ug/code_fission/share_link",
	} {
		for _, action := range actions {
			if action.Path == excluded {
				t.Errorf("需要入参的端点不该进批量入口: %s", excluded)
			}
		}
	}
	// 预算必须严格大于最坏耗时：固定端点各 perRequestTimeout，动态入口
	// （活动表 1 次 + 裂变 status/grant 2 次）各 ugDynamicTimeout，再加一次
	// 活动触发的预留 ugClaimReserve（触发条数不写死，靠 ugHasBudget 截断，
	// 所以「最坏耗时」= 预算本身，恒不超过 ugRoundTimeout）。
	fixedWorst := time.Duration(len(actions)) * perRequestTimeout
	dynamicWorst := time.Duration(1+2)*ugDynamicTimeout + ugClaimReserve
	if worst := fixedWorst + dynamicWorst; ugRoundTimeout <= worst {
		t.Fatalf("ugRoundTimeout = %v，必须大于最坏耗时 %v（固定 %v + 动态 %v）",
			ugRoundTimeout, worst, fixedWorst, dynamicWorst)
	}
	if ugClaimReserve <= ugDynamicTimeout {
		t.Fatalf("ugClaimReserve = %v，必须大于一次动态端点超时 %v，否则会反复起必然超时的请求",
			ugClaimReserve, ugDynamicTimeout)
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if action.Path == "" || action.Label == "" || action.Key == "" {
			t.Errorf("%s: key/path/label 不能为空", action.Key)
		}
		if seen[action.Key] {
			t.Errorf("入口 key 重复: %s", action.Key)
		}
		seen[action.Key] = true
	}
}

// TestClaimUGClassifiesEachEndpoint 验证：逐个端点的归类。
func TestClaimUGClassifiesEachEndpoint(t *testing.T) {
	type reply struct {
		status int
		body   string
	}
	// 批量入口只剩两个端点，所以分两轮跑：一轮覆盖「领到 / 活动关闭」，
	// 一轮覆盖「已领取 / 缺参」。第二轮在 lottery 上借用 share_link 的
	// 缺参码 4001，是为了把 ugParamMarkers 那条路径也走到（归类逻辑同源，
	// 与端点无关）。
	rounds := []struct {
		name    string
		replies map[string]reply
		want    map[string]UGStatus
	}{
		{
			name: "正常发放",
			replies: map[string]reply{
				EpUGCompetitionClaim: {200, `{"code":0,"message":"ok"}`},
				EpUGWorkLotteryDraw:  {200, `{"code":2002,"message":"活动已结束"}`},
			},
			want: map[string]UGStatus{
				"competition_claim": UGClaimed,
				"work_lottery_draw": UGUnavailable,
			},
		},
		{
			name: "已领取与缺参",
			replies: map[string]reply{
				EpUGCompetitionClaim: {200, `{"code":1001,"message":"该奖励已领取"}`},
				EpUGWorkLotteryDraw:  {200, `{"code":4001,"message":"param is invalid"}`},
			},
			want: map[string]UGStatus{
				"competition_claim": UGAlready,
				"work_lottery_draw": UGUnavailable,
			},
		},
	}

	for _, round := range rounds {
		replies := round.replies
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				t.Errorf("%s: method = %s, 期望 POST", r.URL.Path, r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT test-access-token" {
				t.Errorf("%s: Authorization = %q", r.URL.Path, got)
			}
			if got := r.Header.Get("X-Market-User-Id"); got == "" {
				t.Errorf("%s: 缺少派生的 X-Market-User-Id", r.URL.Path)
			}
			item, ok := replies[r.URL.Path]
			if !ok {
				t.Errorf("批量领取打到了未知端点 %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(item.status)
			_, _ = w.Write([]byte(item.body))
		}))

		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("%s: ClaimUG 返回错误: %v", round.name, errClaim)
		}
		if len(results) != 2 {
			t.Fatalf("%s: 结果数 = %d, 期望 2", round.name, len(results))
		}
		for _, result := range results {
			status, known := round.want[result.Key]
			if !known {
				t.Errorf("%s: 出现预期外的入口 key: %s", round.name, result.Key)
				continue
			}
			if result.Status != status {
				t.Errorf("%s: %s status = %d, 期望 %d（文案 %q）",
					round.name, result.Key, result.Status, status, result.Message)
			}
		}
	}
}

// TestClaimUGAbortsOnCancelledContext 验证：取消后不再继续打后面的端点。
func TestClaimUGAbortsOnCancelledContext(t *testing.T) {
	stubHost(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, errClaim := ClaimUG(ctx, ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim == nil {
		t.Fatal("取消后应返回错误")
	}
	if calls != 0 {
		t.Errorf("取消后仍打了 %d 个端点", calls)
	}
	if len(results) != 0 {
		t.Errorf("取消后不应有结果，得到 %d 条", len(results))
	}
}

// TestClaimUGRejectsMissingIdentity 验证：没有 uid 的凭证不派发共享设备身份。
func TestClaimUGRejectsMissingIdentity(t *testing.T) {
	credential := ugTestCredential()
	credential.UID = ""
	if _, errClaim := ClaimUG(context.Background(), credential, ugTestConfig(t, "https://example.invalid")); errClaim == nil {
		t.Fatal("缺少 uid 时应报错")
	}
}

// TestClaimUGRejectsGlobalRegion 验证：国际版没有这套活动。
func TestClaimUGRejectsGlobalRegion(t *testing.T) {
	credential := ugTestCredential()
	credential.Region = RegionGlobal
	if _, errClaim := ClaimUG(context.Background(), credential, ConfigFor(RegionGlobal)); errClaim == nil {
		t.Fatal("国际版应拒绝活动福利领取")
	}
}

// TestClaimUGTreatsNonJSONAsFailure 锁住「200 + 非 JSON ≠ 已领取」。
//
// 网关超时页、空 body、{"data":{}} 都没有 code 字段；按零值当成 code=0 会把
// 失败报成「已领取」，用户以为领到了，实际什么都没有。
func TestClaimUGTreatsNonJSONAsFailure(t *testing.T) {
	for _, body := range []string{"<html>502 Bad Gateway</html>", "", `{"data":{}}`} {
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}))
		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("body=%q: 不该中止整轮: %v", body, errClaim)
		}
		for _, result := range results {
			if result.Status != UGFailed {
				t.Fatalf("body=%q: %s status = %d, 期望 UGFailed", body, result.Key, result.Status)
			}
			if strings.TrimSpace(result.Message) == "" {
				t.Fatalf("body=%q: %s 失败时必须带原因", body, result.Key)
			}
		}
	}
}

// TestClaimUGStopsOnUnauthorized 锁住「401 立刻停」：后面的端点只会得到同一个
// 401，继续打只会让日志更难读；已拿到的结果仍要带出去。
func TestClaimUGStopsOnUnauthorized(t *testing.T) {
	stubHost(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":1001,"message":"unauthorized"}`))
	}))
	defer server.Close()

	results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim == nil {
		t.Fatal("401 应中止整轮并上报错误")
	}
	if calls != 1 {
		t.Fatalf("401 后不应继续请求，实际打了 %d 个端点", calls)
	}
	if len(results) != 1 || results[0].Status != UGFailed {
		t.Fatalf("应带回首个失败结果: %+v", results)
	}
}

// TestSummarizeUGCounts 验证：汇总文案的四类计数与逐条文案。
func TestSummarizeUGCounts(t *testing.T) {
	results := []UGResult{
		{Key: "a", Label: "竞赛奖励", Status: UGClaimed},
		{Key: "b", Label: "联合活动奖励", Status: UGAlready},
		{Key: "c", Label: "作品抽奖", Status: UGUnavailable, Message: "活动已结束"},
		{Key: "d", Label: "作品分享奖励", Status: UGFailed, Message: "HTTP 500"},
	}
	text := SummarizeUG(results)
	for _, want := range []string{"领到 1 项", "此前已领 1 项", "暂不可领 1 项", "失败 1 项", "竞赛奖励：已领取"} {
		if !strings.Contains(text, want) {
			t.Errorf("汇总文案缺少 %q：%s", want, text)
		}
	}
	if got := SummarizeUG(nil); got != "没有可领取的活动入口" {
		t.Errorf("空结果文案 = %q", got)
	}
}

// 暂不可领的活动每轮都一样，只列名字，不逐条铺明细。
func TestSummarizeUGKeepsUnavailableCompact(t *testing.T) {
	text := SummarizeUG([]UGResult{
		{Key: "a", Label: "竞赛奖励", Status: UGUnavailable, Message: "活动暂不可用"},
		{Key: "b", Label: "作品抽奖", Status: UGUnavailable, Message: "活动暂不可用"},
	})
	for _, want := range []string{"暂不可领 2 项", "（竞赛奖励、作品抽奖）"} {
		if !strings.Contains(text, want) {
			t.Errorf("汇总文案缺少 %q：%s", want, text)
		}
	}
	if strings.Contains(text, "活动暂不可用") {
		t.Errorf("暂不可领的明细不该铺进汇总：%s", text)
	}
}

// TestClassifyUGMessage 固定归类口径：码表未知，只按文案粗分。
func TestClassifyUGMessage(t *testing.T) {
	cases := []struct {
		message string
		want    UGStatus
	}{
		{"该奖励已领取", UGAlready},
		{"你已经参与过该活动", UGAlready},
		{"活动已结束", UGUnavailable},
		{"活动未开始", UGUnavailable},
		{"当前账号不满足参与条件", UGUnavailable},
		{"活动暂不可用", UGUnavailable},
		{"param is invalid", UGUnavailable},
		{"参数错误", UGUnavailable},
		{"", UGFailed},
	}
	for _, item := range cases {
		if got := classifyUGMessage(item.message); got != item.want {
			t.Errorf("classifyUGMessage(%q) = %d, 期望 %d", item.message, got, item.want)
		}
	}
}

// TestTaskSpecsFollowRegion 验证：只有国内版声明活动福利族，且是手动族。
//
// 不带 DefaultHours 是刻意的：这些入口只在活动期有东西可领，默认排程只会
// 每天白打 2 次空请求。
func TestTaskSpecsFollowRegion(t *testing.T) {
	cn := &adapter{config: ConfigFor(RegionCN)}
	specs := cn.TaskSpecs()
	if len(specs) != 1 || specs[0].Kind != TaskKindUG {
		t.Fatalf("国内版任务族 = %+v, 期望只有 %s", specs, TaskKindUG)
	}
	if !specs[0].Manual {
		t.Error("活动福利必须是手动族")
	}
	if len(specs[0].DefaultHours) != 0 {
		t.Errorf("手动族不应带默认排程，得到 %v", specs[0].DefaultHours)
	}
	if specs[0].Label == "" || specs[0].Description == "" {
		t.Error("任务族必须带中文标签与说明")
	}
	// 宿主只为「Route 非空」的手动族注册管理路由；没有 Route 的手动族在控制台
	// 根本没有触发入口，等于声明了却做不到。
	if specs[0].Route == "" {
		t.Error("手动族必须带 Route，否则控制台没有触发入口")
	}
	if TaskKindUG != "ug" {
		t.Errorf("族名 = %q, 期望与相邻 vendor 一致的 %q", TaskKindUG, "ug")
	}

	global := &adapter{config: ConfigFor(RegionGlobal)}
	if specsGlobal := global.TaskSpecs(); len(specsGlobal) != 0 {
		t.Errorf("国际版不应声明任务族，得到 %+v", specsGlobal)
	}
}

// TestRunScheduledRejectsUnknownKind 验证：未知任务族直接报错，不静默成功。
func TestRunScheduledRejectsUnknownKind(t *testing.T) {
	cn := &adapter{config: ConfigFor(RegionCN)}
	if _, errRun := cn.RunScheduled(context.Background(), nil, "workbuddy_dailybonus"); errRun == nil {
		t.Fatal("未知任务族应报错")
	}
}

// TestClaimUGReadsNoCodeState 锁住「无业务码响应按状态字段归类」。
//
// 实测 claim_package 回 {"eligible":false,...}：没有 code。按 code==nil 一律
// 归失败，会把「当前账号不满足条件」报成上游故障，用户只能看到一串无意义的
// 失败横幅。
//
// 每个用例连文案一起断言：三个判定分支（不满足条件 / 可参与但未发放 / 本次
// 未发放）是用户唯一能看到的输出，改判定时文案错配比状态错更难发现。
func TestClaimUGReadsNoCodeState(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  UGStatus
		message string
	}{
		{"资格不足-顶层", `{"eligible":false,"message":"no quota"}`, UGUnavailable, "当前账号不满足活动条件"},
		{"资格不足-嵌套", `{"data":{"eligible":false}}`, UGUnavailable, "当前账号不满足活动条件"},
		{"本次已发放", `{"eligible":true,"newly_granted":true}`, UGClaimed, "上游本次发放成功"},
		{"可参与但未发放", `{"eligible":true,"newly_granted":false}`, UGUnavailable, "活动可参与，但本次未发放"},
		// 两个字段同时出现且互相矛盾：以「本次发放成功」为准。上游真的发了
		// 东西，却因为 eligible=false 报「不满足条件」，等于把领到的奖励吞掉。
		{"资格不足但本次已发放", `{"eligible":false,"newly_granted":true}`, UGClaimed, "上游本次发放成功"},
		// 只给 newly_granted=false，没有 eligible：本次没发东西，但不是
		// 「不满足条件」，文案不能张冠李戴。
		{"仅本次未发放", `{"newly_granted":false}`, UGUnavailable, "本次未发放"},
		{"本次已发放-嵌套", `{"data":{"eligible":false,"newly_granted":true}}`, UGClaimed, "上游本次发放成功"},
		// 混合形状：顶层只给一个字段、data 层另给一个。必须逐字段回落补齐——
		// 「顶层有任一字段就整层信顶层」会把 data 里的 newly_granted 丢掉，
		// 把已发放读成「可参与但本次未发放」。
		{"混合-顶层eligible+data发放", `{"eligible":true,"data":{"newly_granted":true}}`, UGClaimed, "上游本次发放成功"},
		{"混合-顶层未发放+data不满足", `{"newly_granted":false,"data":{"eligible":false}}`, UGUnavailable, "当前账号不满足活动条件"},
	}
	for _, item := range cases {
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(item.body))
		}))
		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("%s: 不该中止整轮: %v", item.name, errClaim)
		}
		for _, result := range results {
			if result.Status != item.status {
				t.Fatalf("%s: %s status = %d, 期望 %d", item.name, result.Key, result.Status, item.status)
			}
			if result.Message != item.message {
				t.Fatalf("%s: %s message = %q, 期望 %q", item.name, result.Key, result.Message, item.message)
			}
			// 这条路径上游没给业务码，Code 是零值占位——所以只能按 Status 判，
			// 不能按 Code==0 判「上游回 0 = 已领取」。
			if result.Code != 0 {
				t.Fatalf("%s: %s Code = %d, 这条路径不该有业务码", item.name, result.Key, result.Code)
			}
		}
	}
}

// TestClaimUGClassifiesMissingParamOnNon200 锁住「非 200 的缺参数也归暂不可领」。
//
// 实测 cobrand/claim 缺 activity_id 回 400 + code 9080「param is invalid」。
func TestClaimUGClassifiesMissingParamOnNon200(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":9080,"message":"param is invalid"}`))
	}))
	defer server.Close()

	results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim != nil {
		t.Fatalf("400 不该中止整轮: %v", errClaim)
	}
	for _, result := range results {
		if result.Status != UGUnavailable {
			t.Fatalf("%s status = %d, 期望 UGUnavailable（文案 %q）", result.Key, result.Status, result.Message)
		}
		if result.Code != 9080 {
			t.Errorf("%s code = %d, 期望原样带出 9080", result.Key, result.Code)
		}
	}
}

// TestClaimUGKeepsServerErrorsAsFailure 锁住「5xx / 429 不能被文案折成暂不可领」。
//
// 这是本函数原先的缺陷：非 200 分支只要文案命中「暂不可用」就归 UGUnavailable，
// 于是一个 503（上游真的挂了）会被显示成「活动暂不可领」，调度不会重试、用户
// 也不会，故障被静默吞掉。归类只对 4xx 成立；5xx 与 429 必须留给 Classify。
func TestClaimUGKeepsServerErrorsAsFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"503-文案像活动关闭", http.StatusServiceUnavailable, `{"code":5001,"message":"活动暂不可用"}`},
		{"503-纯网关页", http.StatusBadGateway, `<html>502 Bad Gateway</html>`},
		{"500-缺参文案", http.StatusInternalServerError, `{"code":9080,"message":"param is invalid"}`},
		{"429-已领取文案", http.StatusTooManyRequests, `{"code":1001,"message":"该奖励已领取"}`},
	}
	for _, item := range cases {
		stubHost(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ugNeutralEndpoint(w, r) {
				return
			}
			w.WriteHeader(item.status)
			_, _ = w.Write([]byte(item.body))
		}))
		results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
		server.Close()
		if errClaim != nil {
			t.Fatalf("%s: 不该中止整轮: %v", item.name, errClaim)
		}
		if len(results) != 2 {
			t.Fatalf("%s: 结果数 = %d, 期望 2", item.name, len(results))
		}
		for _, result := range results {
			if result.Status != UGFailed {
				t.Errorf("%s: %s status = %d, 期望 UGFailed（文案 %q）", item.name, result.Key, result.Status, result.Message)
			}
			if strings.TrimSpace(result.Message) == "" {
				t.Errorf("%s: %s 失败时必须带原因", item.name, result.Key)
			}
		}
	}
}

// TestClaimUGClassifiesAlreadyOnNon200 锁住「4xx 的已领取也归已领取」。
//
// 实测幂等端点对重复领取回 409 + 1001「该奖励已领取」。只认 UGUnavailable
// 会让这种正常结果掉进「失败」，用户看到红字却什么都没坏。
func TestClaimUGClassifiesAlreadyOnNon200(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":1001,"message":"该奖励已领取"}`))
	}))
	defer server.Close()

	results, errClaim := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if errClaim != nil {
		t.Fatalf("409 不该中止整轮: %v", errClaim)
	}
	for _, result := range results {
		if result.Status != UGAlready {
			t.Errorf("%s status = %d, 期望 UGAlready（文案 %q）", result.Key, result.Status, result.Message)
		}
		if result.Code != 1001 {
			t.Errorf("%s code = %d, 期望原样带出 1001", result.Key, result.Code)
		}
	}
}

// TestReadUGState 验证：没有状态字段时必须退回失败归类，不能被当成可领取。
func TestReadUGState(t *testing.T) {
	for _, body := range []string{`{"data":{}}`, `<html>502</html>`, `{}`, `null`} {
		if _, okState := readUGState([]byte(body)); okState {
			t.Errorf("body=%q: 不该识别出状态字段", body)
		}
	}
}

// TestUGDetailText 验证：机器文案翻成中文提示，认不出的原样返回。
//
// 只覆盖可达输入：需要活动/作品 ID 的三个端点已不在批量入口里，缺参类文案
// （param is invalid / 9080）不会再出现在结果中，所以那条翻译分支已删除。
// 这里仍保留一条「不可达也不能乱翻」的断言，防止有人日后顺手加回翻译。
func TestUGDetailText(t *testing.T) {
	cases := map[string]string{
		"活动暂不可用":           "活动暂不可用",
		"活动暂未开放":           "活动暂不可用",
		"HTTP 500 服务繁忙":    "HTTP 500 服务繁忙",
		"param is invalid": "param is invalid",
	}
	for message, want := range cases {
		if got := ugDetailText(message); got != want {
			t.Errorf("ugDetailText(%q) = %q, 期望 %q", message, got, want)
		}
	}
}

// TestTaskSpecsDeclareUGForCNOnly 锁住 Trae 家族对外声明的任务族内容。
//
// 宿主把 TaskSpec 原样交给控制台渲染：Label / Description 写错，用户看到的
// 就是错的；Route 丢了，手动触发入口整个消失（admin 跳过 Route 为空的手动
// 族）；Manual 丢了，一个「不是每天都有东西可领」的活动会被排程每天空打。
// 国际版没有这套活动，必须返回 nil 而不是一个空族。
func TestTaskSpecsDeclareUGForCNOnly(t *testing.T) {
	specs := (&adapter{config: ConfigFor(RegionCN)}).TaskSpecs()
	if len(specs) != 1 {
		t.Fatalf("国内版任务族数 = %d, 期望 1", len(specs))
	}
	spec := specs[0]
	if spec.Kind != TaskKindUG {
		t.Errorf("Kind = %q, 期望 %q", spec.Kind, TaskKindUG)
	}
	if spec.Label != "活动福利领取" {
		t.Errorf("Label = %q（控制台原样展示）", spec.Label)
	}
	if spec.Description != "领取 Trae 国内版的竞赛奖励与作品抽奖" {
		t.Errorf("Description = %q（控制台原样展示，改文案要同步这里）", spec.Description)
	}
	if spec.Route != "/ug/claim" {
		t.Errorf("Route = %q, 期望 /ug/claim（缺了控制台不出现手动触发入口）", spec.Route)
	}
	if !spec.Manual {
		t.Error("Manual = false，活动福利是手动族")
	}
	if len(spec.DefaultHours) != 0 {
		t.Errorf("DefaultHours = %v, 期望空（活动不是每天都有东西可领）", spec.DefaultHours)
	}

	if global := (&adapter{config: ConfigFor(RegionGlobal)}).TaskSpecs(); global != nil {
		t.Errorf("国际版任务族 = %v, 期望 nil（国际版没有这套活动）", global)
	}
}

// ugNeutralEndpoint 让「只看固定端点」的用例不受动态入口干扰。
//
// ClaimUG 现在还会读活动表（activity/info）并探测裂变期（code_fission/
// share_status）。这两个入口有各自的用例；这里给它们一份「空表 + 这期不存在」
// 的默认回复，避免同一份 body 同时喂给三个端点、把断言搅成说不清哪条路径失败。
func ugNeutralEndpoint(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case EpUGActivityInfo:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"commercial_activities":[]}`))
		return true
	case EpCodeFissionShareStatus:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":9090,"message":"活动暂不可用"}`))
		return true
	}
	return false
}

// ugCallLog 记录假上游收到的路径。handler 跑在独立 goroutine 里，加锁是必需的。
type ugCallLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *ugCallLog) add(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, path)
}

func (l *ugCallLog) count(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, p := range l.paths {
		if p == path {
			n++
		}
	}
	return n
}

// ugStub 造一个假上游：routes 里有的路径回给定 body，其余走中性回复。
func ugStub(routes map[string]string, log *ugCallLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if log != nil {
			log.add(r.URL.Path)
		}
		if body, ok := routes[r.URL.Path]; ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}
}

// ugResultByKey 按 key 索引结果，key 重复直接判失败。
func ugResultByKey(t *testing.T, results []UGResult) map[string]UGResult {
	t.Helper()
	byKey := make(map[string]UGResult, len(results))
	for _, result := range results {
		if _, dup := byKey[result.Key]; dup {
			t.Fatalf("结果 key 重复：%s", result.Key)
		}
		byKey[result.Key] = result
	}
	return byKey
}

// 活动表的五条覆盖了真实响应的全部形态：已发放、可领、自动发放、签到、已结束。
const ugActivityTableFixture = `{"commercial_activities":[
	{"activity_id":"new_user_202609","activity_type":102,"enabled":true,"work_extra":{"granted":true,"grantable":false}},
	{"activity_id":"checkin_202610","activity_type":104,"enabled":true,"work_extra":{"granted":false,"grantable":true}},
	{"activity_id":"send_msg_202610","activity_type":105,"enabled":true,"work_extra":{"auto_grant":true,"granted":false,"grantable":false}},
	{"activity_id":"student_202610","activity_type":107,"enabled":true,"work_extra":{"granted":false,"grantable":true,"general_credits":500}},
	{"activity_id":"ended_202608","activity_type":103,"enabled":true,"end_time_ms":1700000000000,"work_extra":{"granted":false,"grantable":true}}
]}`

// 实测 student_auth_reward 在 activity/info 里 grantable=true，但 action 只回
// 200 `{}`、复查 granted 仍是 false —— 上游要的是先完成学籍认证。这类「要用户
// 自己去认证 / 装桌面端 / 发消息」的活动一律不打 action，也不当成福利推出来。
func TestClaimUGSkipsActivitiesNeedingManualAction(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpUGActivityInfo:   ugActivityTableFixture,
		EpUGActivityAction: `{"code":0,"message":"发放成功"}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	if got := log.count(EpUGActivityAction); got != 0 {
		t.Fatalf("activity/action 调了 %d 次，want 0（学生认证/发消息都要用户自己动手，插件不该打）", got)
	}
	byKey := ugResultByKey(t, results)
	for _, key := range []string{"activity:student_202610", "activity:send_msg_202610"} {
		if _, ok := byKey[key]; ok {
			t.Fatalf("%s 需要用户自己动手，不该出现在结果里", key)
		}
	}
	if r, ok := byKey["activity:new_user_202609"]; !ok || r.Status != UGAlready {
		t.Fatalf("已发放的新人活动应报已领取：%+v", r)
	}
	if r, ok := byKey["activity:checkin_202610"]; !ok || !strings.Contains(r.Message, "签到") {
		t.Fatalf("签到类活动应指向每日签到：%+v", r)
	}
	if _, ok := byKey["activity:ended_202608"]; ok {
		t.Fatal("已结束的活动不该出现在结果里")
	}
}

// 一键能领的必须领干净：不按固定条数截断，一轮里有几条可领就领几条。
func TestClaimUGClaimsEveryClaimableActivity(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpUGActivityInfo:   ugClaimableTableFixture,
		EpUGActivityAction: `{"code":0,"message":"发放成功"}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	if got := log.count(EpUGActivityAction); got != 3 {
		t.Fatalf("activity/action 调了 %d 次，want 3（三条都可一键领）", got)
	}
	claimed := 0
	for _, result := range results {
		// 固定端点那两条也算领到，这里只看活动表驱动的三条。
		if result.Status == UGClaimed && strings.HasPrefix(result.Key, "activity:") {
			claimed++
		}
	}
	if claimed != 3 {
		t.Fatalf("活动领到 %d 项，want 3：%+v", claimed, results)
	}
}

const ugClaimableTableFixture = `{"commercial_activities":[
	{"activity_id":"promo_a","activity_type":101,"enabled":true,"work_extra":{"granted":false,"grantable":true,"general_credits":100}},
	{"activity_id":"promo_b","activity_type":101,"enabled":true,"work_extra":{"granted":false,"grantable":true,"general_credits":200}},
	{"activity_id":"promo_c","activity_type":101,"enabled":true,"work_extra":{"granted":false,"grantable":true,"work_credits":300}}
]}`

func TestClaimUGActivityActionNeedsPrereq(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(ugStub(map[string]string{
		EpUGActivityInfo:   ugClaimableTableFixture,
		EpUGActivityAction: `{}`,
	}, nil))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	result := ugResultByKey(t, results)["activity:promo_a"]
	if result.Status != UGNeedsPrereq {
		t.Fatalf("200 空对象应判为需先完成前置条件，得到 %v：%s", result.Status, result.Message)
	}
	if strings.TrimSpace(result.Message) == "" {
		t.Fatal("需前置的结果必须给出说明，不能是空文案")
	}
}

func TestClaimUGActivityInfoFailureKeepsFixedResults(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpUGActivityInfo {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"message":"内部错误"}`))
			return
		}
		if r.URL.Path == EpCodeFissionShareStatus {
			// 这期没有裂变活动：结果里只该剩固定端点那两条。
			_, _ = w.Write([]byte(`{"code":9090,"message":"活动暂不可用"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("活动表故障不该让整轮失败：%v", err)
	}
	if len(results) != len(UGActions()) {
		t.Fatalf("活动表故障时应只剩固定端点结果，得到 %d 条：%+v", len(results), results)
	}
}

func TestClaimUGFissionReportsInvitees(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"code":0,"invitees":[{"invitee_name":"a","inviter_reward_credits":100,"status":2}]}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	result := ugResultByKey(t, results)["fission:"+fissionActivityCandidates[0]]
	if result.Status != UGAlready || !strings.Contains(result.Message, "累计 100 积分") {
		t.Fatalf("裂变进度不对：%v / %s", result.Status, result.Message)
	}
	if got := log.count(EpCodeFissionGrant); got != 0 {
		t.Fatalf("没有待发放的邀请时不该调 grant，实际 %d 次", got)
	}
}

func TestClaimUGFissionGrantsPendingInvitee(t *testing.T) {
	stubHost(t)
	log := &ugCallLog{}
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"code":0,"invitees":[{"invitee_name":"a","inviter_reward_credits":100,"status":1}]}`,
		EpCodeFissionGrant:       `{"code":0,"message":"发放成功"}`,
	}, log))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	byKey := ugResultByKey(t, results)
	if r := byKey["fission:"+fissionActivityCandidates[0]]; r.Status != UGUnavailable || !strings.Contains(r.Message, "待发放") {
		t.Fatalf("有待发放邀请时进度应如实说明：%v / %s", r.Status, r.Message)
	}
	if got := log.count(EpCodeFissionGrant); got != 1 {
		t.Fatalf("有待发放邀请时应调 grant 一次，实际 %d 次", got)
	}
	if r := byKey["fission-grant:"+fissionActivityCandidates[0]]; r.Status != UGClaimed {
		t.Fatalf("grant 成功应记为已领取：%+v", r)
	}
}

func TestClaimUGFissionShowsInviteCodeWhenNoInvitees(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"code":0,"invitees":[]}`,
		EpCodeFissionShareLink:   `{"code":0,"invite_code":"ABC123","share_url":"https://example.invalid/s/abc"}`,
	}, nil))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	result := ugResultByKey(t, results)["fission:"+fissionActivityCandidates[0]]
	if !strings.Contains(result.Message, "ABC123") {
		t.Fatalf("还没有被邀请人时应把邀请码带出来：%q", result.Message)
	}
}

func TestClaimUGFissionIgnoresReplyWithoutBusinessCode(t *testing.T) {
	stubHost(t)
	// 200 + `{"data":{}}`：网关兜页、被中间层改写都长这样。缺 code 不能当
	// 「这一期存在」，否则会把一张空壳报成活动进行中。
	server := httptest.NewServer(ugStub(map[string]string{
		EpCodeFissionShareStatus: `{"data":{}}`,
	}, nil))
	defer server.Close()

	results, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL))
	if err != nil {
		t.Fatalf("ClaimUG 出错：%v", err)
	}
	if _, ok := ugResultByKey(t, results)["fission:"+fissionActivityCandidates[0]]; ok {
		t.Fatal("没有业务码的响应不该被当成裂变期")
	}
}

func TestClaimUGFissionCredentialFailureStops(t *testing.T) {
	stubHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCodeFissionShareStatus {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"登录已过期"}`))
			return
		}
		if ugNeutralEndpoint(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}))
	defer server.Close()

	if _, err := ClaimUG(context.Background(), ugTestCredential(), ugTestConfig(t, server.URL)); err == nil {
		t.Fatal("凭证失效应让整轮报错，而不是静默跳过裂变")
	}
}

func TestSummarizeUGCountsPrereqAsPending(t *testing.T) {
	summary := SummarizeUG([]UGResult{
		{Key: "claim_package", Label: "每日 200 积分", Status: UGClaimed, Message: "领到 200 积分"},
		{Key: "activity:student_202610", Label: "学生认证 500 积分", Status: UGNeedsPrereq, Message: "需先完成学生认证"},
	})
	if !strings.Contains(summary, "待满足前置 1 项") {
		t.Fatalf("需前置的活动应单列计数：%s", summary)
	}
	if !strings.Contains(summary, "失败 0 项") {
		t.Fatalf("需前置不该计入失败：%s", summary)
	}
}
