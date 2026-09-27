package opencodezen

// 本文件实现 OpenCode ZEN 的额度查询。
//
// **ZEN 上游不提供额度接口**（实测：其官方网关项目只有本地 token 计数，
// 没有任何余额/配额端点）。因此这里不编数字，返回明确的「不支持」——
// 页面会显示 "—"，用户知道去 OpenCode 订阅页看真实额度。
//
// 保留本文件而不是把逻辑塞进适配层：三个供应商的额度能力都叫 quota.go，
// 读代码的人不必先猜「这家把额度写在哪」。

import (
	"errors"
)

// ErrQuotaUnsupported 表示本供应商没有额度查询能力。
var ErrQuotaUnsupported = errors.New("opencodezen upstream does not expose a quota endpoint")
