package core

import (
	"strings"
	"sync"
)

// CreditTable 是「模型 ID → 计费倍率文案」的并发安全表。
//
// 各供应商给宿主提供倍率只有一个形态：探测时整表替换，控制台读取时整表快照。
// 这段逻辑原本在 cline / qoder / raccoon / autoclaw / trae / opencodezen 各抄
// 一份（连字段名都一样），统一收在这里。零值可用。
type CreditTable struct {
	mu      sync.RWMutex
	byModel map[string]string
}

// Replace 用一轮探测的结果整表替换。
//
// nil 或空表表示「这一轮没有倍率」，读取端会拿到 nil 并显示「—」，而不是
// 保留上一轮的陈旧数据。
func (t *CreditTable) Replace(credits map[string]string) {
	next := make(map[string]string, len(credits))
	for id, label := range credits {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if label = strings.TrimSpace(label); label != "" {
			next[id] = label
		}
	}
	t.mu.Lock()
	t.byModel = next
	t.mu.Unlock()
}

// FillCredits 把一批模型的倍率文案归一化后整表写入 t：id 取对外模型 ID，
// label 取上游原始文案（缺失返回空串）。空 ID、空文案都跳过——未知不写进表。
//
// 「遍历模型 → 归一化 → 整表替换」原本在六家供应商各抄了一份（连字段名都
// 一样），收在这里之后，各家只提供两个取字段的函数。
func FillCredits[T any](t *CreditTable, models []T, id func(T) string, label func(T) string) {
	next := make(map[string]string, len(models))
	for _, model := range models {
		modelID := strings.TrimSpace(id(model))
		if modelID == "" {
			continue
		}
		raw := strings.TrimSpace(label(model))
		if raw == "" {
			continue
		}
		if rate := FormatRate(raw); rate != "" {
			next[modelID] = rate
		}
	}
	t.Replace(next)
}

// Snapshot 返回整表副本；空表返回 nil，调用方据此判定「未知」。
func (t *CreditTable) Snapshot() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.byModel) == 0 {
		return nil
	}
	out := make(map[string]string, len(t.byModel))
	for id, label := range t.byModel {
		out[id] = label
	}
	return out
}
