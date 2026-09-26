package main

// 本文件是 gateway_hint 文案的单一来源。
//
// 提示文案的价值在于把「上游返回了一个语焉不详的错误码」翻译成
// 「你该做什么」。没有它，用户看到 400 + 一串中文只能去翻文档。
//
// 判定次序：业务码（11133/11135）先于通用分类——它们更具体。

import (
	"strings"

	"workbuddy2api-plugin/internal/cb"
)

// gatewayHint 返回给某个错误码附加的提示文案（没有则返回空串）。
func gatewayHint(code string, prepared preparedExecution) string {
	trimmed := strings.TrimSpace(code)
	switch trimmed {
	case "11133":
		// 参数被拒：区分「模型不支持图片」与「参数格式问题」，前者可指导换模型。
		if cb.HasImagePart(prepared.rpc.Payload) && !modelSupportsImages(prepared) {
			return "model " + prepared.model + " does not support images; pick one with image support from /v1/models"
		}
		return "request parameters were rejected by the model provider; check message format and model capabilities"
	case "11135":
		return "image data rejected by upstream; use a real/valid image, may need a new conversation"
	case "11115":
		return "request context exceeds the model's limit; reduce history or message size"
	case cbModelRateLimitCode:
		return "this model is temporarily rate-limited upstream; retry later or switch model"
	case cbModelBlockCode:
		return "upstream has no such model on this backend; switch model or retry on another account"
	}
	return ""
}

// 上游业务码常量（提示判定用）。
const (
	cbModelRateLimitCode = "6004"
	cbModelBlockCode     = "11102"
)

// modelSupportsImages 报告当前请求的模型是否声明支持图片。
//
// 查的是已发布的模型目录：查不到时保守返回 true（不误导用户换模型）。
func modelSupportsImages(prepared preparedExecution) bool {
	cached := cachedModelsForRealm(prepared.region)
	for _, model := range cached {
		if model.ID == prepared.model {
			return model.SupportsImages
		}
	}
	return true
}

// cachedModelsForRealm 取某个域已缓存的模型清单（不触发上游请求）。
func cachedModelsForRealm(region cb.Region) []cb.ModelInfo {
	return cb.CachedModels()[region]
}
