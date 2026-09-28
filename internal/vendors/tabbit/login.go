package tabbit

// 登录相关实现：Tabbit 采用 API Key 或 Token 方式配置。

import (
	"context"
	"errors"

	"freetier2api-plugin/cpasdk/pluginapi"
)

var ErrLoginUnsupported = errors.New("tabbit 不支持网页登录，请通过「添加 API Key」或凭证 JSON 方式添加")

func SupportsLogin() bool {
	return false
}

func LoginStart(ctx context.Context) (*pluginapi.AuthLoginStartResponse, error) {
	return nil, ErrLoginUnsupported
}

func LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return nil, ErrLoginUnsupported
}
