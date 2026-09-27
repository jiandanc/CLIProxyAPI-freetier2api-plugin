package opencodezen

// 本文件实现 OpenCode ZEN 的凭证校验（续期）。
//
// 与 OAuth 供应商的本质差异：ZEN 的 API key **不会轮换**——它是一把长期
// 静态凭证，没有 refresh token 可换。因此这里的「续期」只是**校验可用性**：
// 列一次模型能验证 key 是否仍被上游接受。
//
// 保留本文件而不是省略：三个供应商的续期能力都叫 refresh.go。

import (
	"context"
	"strings"
)

// VerifyCredential 校验 API key 是否仍然可用。
//
// 返回 nil 表示 key 有效。校验方式是列一次模型——这是最轻量的、确实需要
// 认证的上游调用；比直接发一次对话请求便宜得多。
//
// 不做任何写回：key 不会变，重写文件只会让 mtime 无意义地变动，
// 用户无法从文件时间判断账号是否真的被上游重新确认过。
func VerifyCredential(ctx context.Context, baseURL, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return ErrMissingAPIKey
	}
	_, errModels := FetchModels(ctx, baseURL, apiKey)
	return errModels
}
