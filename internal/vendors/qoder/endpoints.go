package qoder

// 本文件是 Qoder 的端点表面向本包的入口。
//
// 端点表的**真正的定义**在 qoderapi 子包（endpoints.go）：Qoder 的区域类型
// Region 与端点表同处一地，且被 bridge / cosy 等子包广泛使用，不适合搬动。
// 这里做一层转发，让「端点表在 endpoints.go」的约定在本包也成立——
// 四个供应商的 endpoints.go 都回答同一个问题：按区域给我端点。

import "freetier2api-plugin/internal/vendors/qoder/qoderapi"

// Region 标识 Qoder 站点区域（别名，与 workbuddy 的 Region 语义一致）。
type Region = qoderapi.Region

const (
	// RegionCN 是国内版。
	RegionCN = qoderapi.RegionCN
	// RegionGlobal 是国际版。
	RegionGlobal = qoderapi.RegionGlobal
)

// NormalizeRegion 将字符串归一化为合法 Region 值。
func NormalizeRegion(s string) Region { return qoderapi.NormalizeRegion(s) }

// GetEndpoints 根据区域返回对应端点配置。
func GetEndpoints(region Region) qoderapi.Endpoints { return qoderapi.GetEndpoints(region) }
