package main

// 本文件封装宿主提供的凭证回调（host.auth.list / host.auth.get）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// hostAuthEntry 是宿主凭证列表里的一条。
//
// 字段与 pluginapi.HostAuthFileEntry 对齐。
type hostAuthEntry struct {
	ID          string    `json:"id"`
	AuthIndex   string    `json:"auth_index"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Label       string    `json:"label"`
	Status      string    `json:"status"`
	Disabled    bool      `json:"disabled"`
	Unavailable bool      `json:"unavailable"`
	RuntimeOnly bool      `json:"runtime_only"`
	Path        string    `json:"path,omitempty"`
	ModTime     time.Time `json:"modtime,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// pluginAuthName 是本插件凭证在宿主里的类型名（对应宿主 auth 记录的 Type）。
const pluginAuthName = providerKey

// listHostAuths 列出本插件名下的全部凭证。
//
// **必须容忍被前置插件抢走的凭证**：宿主遍历插件解析凭证文件时，
// 一旦某个插件报错或认领就中止循环，且会把「当前询问的插件的 identifier」
// 兜底填进 provider。因此归属判定过宽的前置插件（如 qoder2api 判定
// `provider == "qoder"` 即认领）会把本插件的文件抢走，那些账号的 provider
// 变成对方。
//
// 更麻烦的是插件的 priority 可能被宿主覆盖：从插件商店安装/升级时宿主会
// 重写 plugins.configs.<id> 整块配置，手动加的 priority 会被抹掉，
// 顺序又变回按 id 升序（qoder2api < workbuddy2api）。因此**不能只靠优先级**。
//
// 兜底依据：这些凭证文件的文件名仍是 workbuddy-*.json（本插件的命名约定），
// 按名字也属于本插件，因此一并收下——否则用户的合法账号会在页面上凭空消失。
func listHostAuths(ctx context.Context, callbackID string) ([]hostAuthEntry, error) {
	raw, errCall := callHostScoped(callbackID, pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil, fmt.Errorf("host.auth.list: %w", errCall)
	}
	// 宿主可能返回数组，也可能返回带包装的对象；两种都接受。
	var entries []hostAuthEntry
	if errUnmarshal := json.Unmarshal(raw, &entries); errUnmarshal == nil {
		return filterPluginAuths(entries), nil
	}
	var wrapper struct {
		Auths []hostAuthEntry `json:"auths"`
		Files []hostAuthEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", errUnmarshal)
	}
	// 宿主返回的 Auths 是内存运行时实例，Files 是磁盘物理文件。
	// 按文件名将 Files 的文件属性（如 ModTime、Path）合入 Auths，避免将同一凭证重复展示。
	entries = mergeAuthsAndFiles(wrapper.Auths, wrapper.Files)
	return filterPluginAuths(entries), nil
}

// mergeAuthsAndFiles 合并宿主 Auths 与 Files，避免同一凭证出现重复条目。
func mergeAuthsAndFiles(auths, files []hostAuthEntry) []hostAuthEntry {
	if len(files) == 0 {
		return auths
	}
	if len(auths) == 0 {
		return files
	}

	out := make([]hostAuthEntry, len(auths))
	copy(out, auths)
	authIdxByName := make(map[string]int, len(auths))
	for i, a := range out {
		if key := normalizeAuthKey(a.Name); key != "" {
			authIdxByName[key] = i
		}
		if key := normalizeAuthKey(a.ID); key != "" {
			authIdxByName[key] = i
		}
	}

	for _, file := range files {
		key := normalizeAuthKey(file.Name)
		if key == "" {
			key = normalizeAuthKey(file.ID)
		}
		if idx, found := authIdxByName[key]; found {
			mergeHostAuthEntry(&out[idx], file)
		} else {
			out = append(out, file)
		}
	}
	return out
}

// filterPluginAuths 只保留属于本插件的凭证，并对重复条目（如同时出现在 Auths 与 Files 中）进行去重合并。
//
// 判据有两层，命中任一即收下：
//  1. provider 是本插件（正常情况）；
//  2. 文件名符合本插件约定（workbuddy-*.json）——用于兜住被前置插件抢走的
//     凭证。只看 provider 会让这些账号从页面上消失，而用户无从知道原因。
//
// 两层都不会误吞别家凭证：别家的文件名不含 workbuddy。
func filterPluginAuths(entries []hostAuthEntry) []hostAuthEntry {
	out := make([]hostAuthEntry, 0, len(entries))
	indexByKey := make(map[string]int)

	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		belongs := (provider != "" && strings.EqualFold(provider, pluginAuthName)) || fileNameBelongsToPlugin(entry.Name)
		if !belongs {
			continue
		}

		// 归一化去重键：优先用去掉 .json 的 Name，其次用 ID
		key := normalizeAuthKey(entry.Name)
		if key == "" {
			key = normalizeAuthKey(entry.ID)
		}
		if key == "" {
			key = strings.TrimSpace(entry.AuthIndex)
		}

		if idx, exists := indexByKey[key]; exists && key != "" {
			mergeHostAuthEntry(&out[idx], entry)
			continue
		}

		if key != "" {
			indexByKey[key] = len(out)
		}
		out = append(out, entry)
	}
	return out
}

// accountIdentity 返回一个宿主账号条目的**唯一权威身份标识**。
//
// 这是「按账号落状态」的唯一取值点：签到记录、任务历史、额度缓存都必须经它，
// 否则同一个账号会在不同路径上算出不同身份。
//
// 之所以需要它：宿主给同一条账号记录提供了多个标识（ID、Name、AuthIndex），
// 三者并不相等（AuthIndex 是运行期索引，Name 才对应磁盘文件名）。早期代码
// 有的路径传 AuthIndex、有的传 Name，于是「UID 取自文件名」的供应商
// （Qoder/ZCode）在签到与展示上得到两个身份，签到写进 A、页面查 B，
// 永远显示「未签到」。统一收口到本函数后，这类分歧不可能再出现。
//
// 取值优先级：Name（磁盘文件名，登录落盘时生成，可读且稳定）→ ID（宿主
// 稳定标识，运行时账号没有 Name）→ AuthIndex（最后兜底，仅保证非空）。
func accountIdentity(entry hostAuthEntry) string {
	for _, candidate := range []string{entry.Name, entry.ID, entry.AuthIndex} {
		if trimmed := strings.TrimSuffix(strings.TrimSpace(candidate), ".json"); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeAuthKey(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, ".json")
	return s
}

func mergeHostAuthEntry(target *hostAuthEntry, source hostAuthEntry) {
	if target.AuthIndex == "" && source.AuthIndex != "" {
		target.AuthIndex = source.AuthIndex
	}
	if target.Path == "" && source.Path != "" {
		target.Path = source.Path
	}
	if target.ModTime.IsZero() && !source.ModTime.IsZero() {
		target.ModTime = source.ModTime
	}
	if target.Label == "" && source.Label != "" {
		target.Label = source.Label
	}
	if target.Status == "" && source.Status != "" {
		target.Status = source.Status
	}
	if target.RuntimeOnly && !source.RuntimeOnly {
		target.RuntimeOnly = false
	}
}

// fileNameBelongsToPlugin 判断文件名是否符合本插件的命名约定。
//
// 判据来自供应商注册表而不是硬编码前缀：每个供应商实例的 ID 就是它
// 的凭证文件名前缀（workbuddycn / qoderglobal / cline / opencodezen）。
// 新增供应商时它自动生效，不需要在这里同步一份名单。
//
// 兼容旧前缀 workbuddy：历史凭证文件是 workbuddy-<uid>.json，当时还没有
// cn/global 之分。
func fileNameBelongsToPlugin(name string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".json"))
	if base == "" {
		return false
	}
	if base == "workbuddy" {
		return true
	}
	for _, vendor := range core.Vendors() {
		id := strings.ToLower(strings.TrimSpace(vendor.ID()))
		if id == "" {
			continue
		}
		if base == id {
			return true
		}
		for _, separator := range []string{"-", "_", "."} {
			if strings.HasPrefix(base, id+separator) ||
				strings.Contains(base, separator+id) {
				return true
			}
		}
	}
	// 旧前缀 workbuddy（未分区时代）：workbuddy-<uid>.json 与 my-workbuddy.json。
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, "workbuddy"+separator) ||
			strings.Contains(base, separator+"workbuddy") {
			return true
		}
	}
	return false
}

// fetchAuthJSON 按 auth_index 取回凭证 JSON。
//
// 宿主的 host.auth.get 只认 auth_index（按 id 查会失败），
// 因此调用方若是从 id 出发，必须先经列表把 id 映射成 index。
func fetchAuthJSON(ctx context.Context, callbackID, authID string) ([]byte, error) {
	index := strings.TrimSpace(authID)
	if index == "" {
		return nil, fmt.Errorf("auth id is empty")
	}
	// 先直接按 index 查（多数调用方传进来的就是 index）。
	if raw, okFound := getAuthJSONByIndex(ctx, callbackID, index); okFound {
		return raw, nil
	}
	// 回退：把 id 映射成 index 再查。
	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		return nil, errList
	}
	for _, entry := range entries {
		if entry.ID != index && entry.Name != index {
			continue
		}
		if raw, okFound := getAuthJSONByIndex(ctx, callbackID, entry.AuthIndex); okFound {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("auth %s not found", authID)
}

// getAuthJSONAndPathByIndex 按 auth_index 取凭证 JSON 和物理文件路径。
func getAuthJSONAndPathByIndex(ctx context.Context, callbackID, authIndex string) ([]byte, string, bool) {
	trimmed := strings.TrimSpace(authIndex)
	if trimmed == "" {
		return nil, "", false
	}
	raw, errCall := callHostScoped(callbackID, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: trimmed})
	if errCall != nil {
		logHostCallFailure(pluginabi.MethodHostAuthGet, errCall)
		return nil, "", false
	}
	var response pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, "", false
	}
	if len(response.JSON) == 0 {
		return nil, "", false
	}
	return response.JSON, response.Path, true
}

// getAuthJSONByIndex 按 auth_index 取凭证 JSON。
func getAuthJSONByIndex(ctx context.Context, callbackID, authIndex string) ([]byte, bool) {
	raw, _, ok := getAuthJSONAndPathByIndex(ctx, callbackID, authIndex)
	return raw, ok
}

// activeVendorSet 返回当前拥有至少一个可用凭证的供应商 ID 集合。
//
// 用于模型列表过滤：如果某个供应商未添加 auth/key，模型列表既不去上游获取，
// 也不在列表里展示该供应商的模型。
func activeVendorSet(ctx context.Context, callbackID string) map[string]bool {
	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		logger.Debug("activeVendorSet: list host auths failed: %v", errList)
		return map[string]bool{}
	}
	active := make(map[string]bool)
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, _, okRaw := getAuthJSONAndPathByIndex(ctx, callbackID, entry.AuthIndex)
		if !okRaw && entry.Path != "" {
			if r, err := os.ReadFile(entry.Path); err == nil && len(r) > 0 {
				raw = r
				okRaw = true
			}
		}
		if !okRaw || len(raw) == 0 {
			continue
		}
		if credential, okParse := parseVendorCredential(raw, entry.Name, nil); okParse {
			if id := credential.VendorIDValue(); id != "" {
				active[id] = true
			}
		}
	}
	return active
}
