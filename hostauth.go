package main

// 本文件封装宿主提供的凭证回调（host.auth.list / host.auth.get）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api-plugin/cpasdk/pluginabi"
	"workbuddy2api-plugin/cpasdk/pluginapi"
)

// hostAuthEntry 是宿主凭证列表里的一条。
//
// 字段与 pluginapi.HostAuthFileEntry 对齐；这里只声明用得到的部分，
// 其余字段（size、modtime 等）对本插件没有意义。
type hostAuthEntry struct {
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	RuntimeOnly bool   `json:"runtime_only"`
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
	entries = append(wrapper.Auths, wrapper.Files...)
	return filterPluginAuths(entries), nil
}

// filenameHint 是凭证文件名的归属提示（与 cb.RegisterPathHint 一致）。
const filenameHint = "workbuddy"

// filterPluginAuths 只保留属于本插件的凭证。
//
// 判据有两层，命中任一即收下：
//  1. provider 是本插件（正常情况）；
//  2. 文件名符合本插件约定（workbuddy-*.json）——用于兜住被前置插件抢走的
//     凭证。只看 provider 会让这些账号从页面上消失，而用户无从知道原因。
//
// 两层都不会误吞别家凭证：别家的文件名不含 workbuddy。
func filterPluginAuths(entries []hostAuthEntry) []hostAuthEntry {
	out := make([]hostAuthEntry, 0, len(entries))
	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		if provider != "" && strings.EqualFold(provider, pluginAuthName) {
			out = append(out, entry)
			continue
		}
		if fileNameBelongsToPlugin(entry.Name) {
			out = append(out, entry)
		}
	}
	return out
}

// fileNameBelongsToPlugin 判断文件名是否符合本插件的命名约定。
func fileNameBelongsToPlugin(name string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".json"))
	if base == "" {
		return false
	}
	if base == filenameHint {
		return true
	}
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, filenameHint+separator) ||
			strings.Contains(base, separator+filenameHint) {
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

// getAuthJSONByIndex 按 auth_index 取凭证 JSON。
func getAuthJSONByIndex(ctx context.Context, callbackID, authIndex string) ([]byte, bool) {
	trimmed := strings.TrimSpace(authIndex)
	if trimmed == "" {
		return nil, false
	}
	raw, errCall := callHostScoped(callbackID, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: trimmed})
	if errCall != nil {
		logHostCallFailure(pluginabi.MethodHostAuthGet, errCall)
		return nil, false
	}
	var response pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, false
	}
	if len(response.JSON) == 0 {
		return nil, false
	}
	return response.JSON, true
}
