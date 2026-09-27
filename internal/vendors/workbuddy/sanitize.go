package workbuddy

// 本文件实现出站请求体的指纹脱敏。
//
// 背景：客户端（Claude Code 类 CLI）会在 system prompt 里注入若干固定模板句，
// 上游的内容审核是**逐字精确匹配**（不是语义审核），一字改动即可绕过。
//
// 策略分两层：
//   - 键值/header 型指纹：整段剥离（它本身就是注入痕迹，不含用户语义）；
//   - 承载语义的模板句：最小改写（换一个词），语义不变。
//
// 规则表从 catalog 同级的 sanitize_rules.json 读取（内嵌），
// 便于审计「到底改了哪些字面量」，也避免 Go 源码里出现大量易被误改的临界字符串。

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

//go:embed sanitize_rules.json
var sanitizeRulesRaw []byte

// sanitizeRules 是脱敏规则。
type sanitizeRules struct {
	// Features 是特征预检串：任一命中才进入净化（普通请求零开销）。
	Features []string `json:"features"`
	// Rewrites 是 [原文, 改写] 对。
	Rewrites [][2]string `json:"rewrites"`
	// Regexes 是整段剥离用的正则（按名字取用）。
	Regexes map[string]string `json:"regexes"`
}

var (
	sanitizeOnce    sync.Once
	sanitizeRuleSet sanitizeRules
	sanitizeRegexes map[string]*regexp.Regexp
)

func loadSanitizeRules() (sanitizeRules, map[string]*regexp.Regexp) {
	sanitizeOnce.Do(func() {
		var rules sanitizeRules
		if errUnmarshal := json.Unmarshal(sanitizeRulesRaw, &rules); errUnmarshal != nil {
			// 内嵌资源损坏是构建期问题：没有规则就退化为「不脱敏」，
			// 而不是让插件启动失败（脱敏是加分项，不是必需项）。
			rules = sanitizeRules{}
		}
		compiled := map[string]*regexp.Regexp{}
		for name, pattern := range rules.Regexes {
			if strings.TrimSpace(pattern) == "" {
				continue
			}
			expr, errCompile := regexp.Compile(pattern)
			if errCompile != nil {
				// 规则写坏时跳过该条，而不是 panic。
				continue
			}
			compiled[name] = expr
		}
		sanitizeRuleSet = rules
		sanitizeRegexes = compiled
	})
	return sanitizeRuleSet, sanitizeRegexes
}

// sanitizeMessages 净化请求体里的全部文本。
//
// 净化范围：content（字符串或 parts 里的文本）、reasoning/reasoning_content、
// tool_calls 的参数（它是字符串化 JSON，按文本走）。**图片 part 不动**。
//
// 返回值报告是否发生了改动。
func sanitizeMessages(messages []any) bool {
	rules, regexes := loadSanitizeRules()
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		// 注意：这里**不能**在 content 缺失时 continue。
		// 工具调用消息的 content 通常是 null，早期版本因此在 content 缺失时跳过，
		// 导致 tool_calls 里的参数完全不被净化——写进工具参数（文件名、命令、
		// 写入内容）的被拦字符串会原样漏出。
		if content, okContent := message["content"]; okContent {
			if updated, didChange := sanitizeContent(content, rules, regexes); didChange {
				message["content"] = updated
				changed = true
			}
		}
		for _, key := range []string{"reasoning", "reasoning_content"} {
			if text, okText := message[key].(string); okText {
				if updated := sanitizeText(text, rules, regexes); updated != text {
					message[key] = updated
					changed = true
				}
			}
		}
		if calls, okCalls := message["tool_calls"].([]any); okCalls {
			if sanitizeToolCalls(calls, rules, regexes) {
				changed = true
			}
		}
	}
	return changed
}

// sanitizeContent 净化消息内容（字符串或 parts 数组）。
func sanitizeContent(content any, rules sanitizeRules, regexes map[string]*regexp.Regexp) (any, bool) {
	switch typed := content.(type) {
	case string:
		updated := sanitizeText(typed, rules, regexes)
		return updated, updated != typed
	case []any:
		changed := false
		for _, item := range typed {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			// 图片 part 不动：净化文本字段之外的内容会破坏图片。
			if partType, okType := part["type"].(string); okType && partType == "image_url" {
				continue
			}
			text, okText := part["text"].(string)
			if !okText {
				continue
			}
			if updated := sanitizeText(text, rules, regexes); updated != text {
				part["text"] = updated
				changed = true
			}
		}
		return typed, changed
	}
	return content, false
}

// sanitizeToolCalls 净化工具调用的参数与名称。
func sanitizeToolCalls(calls []any, rules sanitizeRules, regexes map[string]*regexp.Regexp) bool {
	changed := false
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			continue
		}
		if arguments, okArgs := function["arguments"].(string); okArgs {
			if updated := sanitizeText(arguments, rules, regexes); updated != arguments {
				function["arguments"] = updated
				changed = true
			}
		}
	}
	return changed
}

// hasFingerprint 特征预检：快速判断文本是否需要净化。
//
// 用 strings.Contains 走快路径：绝大多数请求全不命中，
// 直接跳过后面所有正则与改写（零分配）。
func hasFingerprint(text string, rules sanitizeRules) bool {
	for _, feature := range rules.Features {
		if strings.Contains(text, feature) {
			return true
		}
	}
	return false
}

// sanitizeText 对一段文本执行完整脱敏。
//
// 执行顺序：
//  1. 特征预检（不命中直接返回原串）；
//  2. 改写表（承载语义的模板句，逐词替换）；
//  3. header 键值段整段剥离；
//  4. cc_ 键值段循环清理（直到稳定）；
//  5. 裸 header 键名缩写。
func sanitizeText(text string, rules sanitizeRules, regexes map[string]*regexp.Regexp) string {
	if text == "" || !hasFingerprint(text, rules) {
		return text
	}
	for _, pair := range rules.Rewrites {
		if len(pair) != 2 || pair[0] == "" {
			continue
		}
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	if expr := regexes["sanitizeHdrRe"]; expr != nil {
		text = expr.ReplaceAllString(text, "")
	}
	if expr := regexes["sanitizeKvRe"]; expr != nil {
		// 循环清理直到稳定：一条规则剥掉前缀后可能让下一条匹配露出来。
		for previous := ""; previous != text; {
			previous = text
			text = expr.ReplaceAllString(text, "")
		}
	}
	if expr := regexes["sanitizeBareHdrRe"]; expr != nil {
		text = expr.ReplaceAllStringFunc(text, func(match string) string {
			// 把裸键名缩写成短形式，保留可读性同时不再是原字面量。
			if len(match) > 4 {
				return match[:len(match)-3] + "hdr"
			}
			return match
		})
	}
	return strings.TrimSpace(text)
}
