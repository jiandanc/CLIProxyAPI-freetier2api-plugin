package zcode

// 本文件实现 ZCode 的签到。
//
// ZCode 的「签到」实质是领取限时活动套餐（billing/preview → billing/claim），
// 而上游对 billing/claim **强制要求 X-Aliyun-Captcha-Verify-Param**：
//
//	POST /api/v1/zcode-plan/billing/claim
//	→ HTTP 400 {"code":3007,"msg":"captcha verify failed"}
//
// 该验证码是阿里云**无痕验证**（traceless）——SDK 在客户端静默采集浏览器与
// 设备信号后由服务端判定风险，通过即回调下发 token。它**没有图片、滑块或
// 字符**可供作答，因此无法用视觉模型代答；参考实现也实测过纯模拟环境
// （happy-dom）路线，2026-09 起被上游以「unusual activity」全拒。唯一可行
// 方案是真浏览器（需下载约 200MB 的补丁 Chromium），本插件不引入这种依赖。
//
// 因此 ZCode 与 Cline / OpenCode ZEN 同属「无签到活动」的供应商：与其让
// 页面提供一个点了必然失败的动作，不如如实声明不支持——控制台会按
// 「无签到活动」展示（见 ui.go 的签到列渲染）。
//
// 额度查询**不受影响**：billing/current 与 billing/balance 不需要验证码，
// 见 quota.go。
//
// 保留本文件而不是删除：所有供应商的签到能力都叫 checkin.go，新增供应商时
// 照抄文件名即可，不必先确认「这家有没有这个功能」。

import "errors"

// ErrCheckinUnsupported 表示本供应商没有签到活动。
var ErrCheckinUnsupported = errors.New("zcode does not provide a daily check-in")
