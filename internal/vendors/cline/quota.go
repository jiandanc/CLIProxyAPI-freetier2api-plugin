package cline

// 本文件实现 Cline 的额度查询。
//
// **Cline 上游不提供额度接口**（实测：cline2api 里只有本地 token 统计与
// 429 冷却推算，没有任何余额/订阅端点）。因此这里不编数字，返回明确的
// 「不支持」——页面会显示 "—"，用户知道去 Cline 的订阅页看真实额度。

import "errors"

// ErrQuotaUnsupported 表示本供应商没有额度查询能力。
var ErrQuotaUnsupported = errors.New("cline upstream does not expose a quota endpoint")
