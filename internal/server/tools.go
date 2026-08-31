// 工具调用（Function Calling）模拟层。
//
// 上游 /v1/chat/chat 没有用户自定义 function-calling 通道，因此在网关侧
// 用「提示词注入 + 输出解析」模拟 OpenAI tools 语义：
//   - 请求带 tools 时，把工具 schema 注入提示词，要求模型用 ```tool_call 代码块回答；
//   - 响应文本里解析 tool_call 块，转换回 OpenAI tool_calls（流式按 index 增量、
//     非流式挂 message.tool_calls，finish_reason=tool_calls）。
//
// 与 workbuddy2api/traework2api 的原生透传相比，这是对无原生支持上游的标准做法。
package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolSpec 归一化后的工具定义。

type toolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// normalizeTools 从 OpenAI tools 数组提取 function 定义。
func normalizeTools(raw []map[string]any) []toolSpec {
	var out []toolSpec
	for _, t := range raw {
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		spec := toolSpec{Name: name}
		spec.Description, _ = fn["description"].(string)
		if p, ok := fn["parameters"]; ok {
			if raw, err := json.Marshal(p); err == nil {
				spec.Parameters = raw
			}
		}
		if len(spec.Parameters) == 0 {
			spec.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, spec)
	}
	return out
}

// toolsActive 判断本次请求是否启用工具模拟。
func toolsActive(req *chatRequest) bool {
	return len(normalizeTools(req.Tools)) > 0 && req.ToolChoice.Mode != "none"
}

// buildToolsPrompt 生成注入提示词的工具说明块。
func buildToolsPrompt(specs []toolSpec, choice toolChoiceOpenAI) string {
	var sb strings.Builder
	sb.WriteString("\n\n# 工具调用（重要）\n")
	sb.WriteString("你可以调用下列工具。需要调用时，在回复中输出一个 ```tool_call 代码块，格式严格如下：\n")
	sb.WriteString("```tool_call\n{\"name\":\"<工具名>\",\"arguments\":{<参数JSON>}}\n```\n")
	sb.WriteString("可输出多个 tool_call 块调用多个工具；代码块之外不要复述工具调用内容。")
	sb.WriteString("不需要调用工具时正常回复文本，绝对不要输出 tool_call 代码块。\n")
	sb.WriteString("arguments 必须是合法 JSON 字符串：反斜杠与双引号必须转义（Windows 路径建议写成正斜杠，如 C:/Users/x 而非 C:\\\\Users\\\\x）。\n")
	sb.WriteString("调用对象只允许 name 与 arguments 两个字段，禁止添加 description 等其他字段；arguments 必须包含工具 parameters 里的全部必填参数。\n")
	sb.WriteString("每个调用必须各自用独立的 ```tool_call 块包裹（先 ``` 闭合再开下一块），不要把多个调用塞进同一块。\n")
	sb.WriteString("绝对不要编造工具输出：工具结果只会由系统以 [工具 xxx 返回结果] 形式提供，出现之前不得假设任何结果。\n")
	switch choice.Mode {
	case "required":
		sb.WriteString("本轮【必须】至少调用一个工具，禁止直接文本回答。\n")
	case "function":
		sb.WriteString(fmt.Sprintf("本轮【必须】调用工具 %q。\n", choice.Function))
	}
	sb.WriteString("工具列表：\n")
	for i, s := range specs {
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, s.Name))
		if s.Description != "" {
			sb.WriteString(" — " + s.Description)
		}
		sb.WriteString("\n   parameters: " + string(s.Parameters) + "\n")
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 输出解析
// ---------------------------------------------------------------------------

// extractToolCalls 从模型输出中提取工具调用。
// 返回解析到的调用、剥离调用块后的剩余文本、是否找到。

// toOpenAIToolCalls 转 OpenAI 响应线格式的 tool_calls。
func toOpenAIToolCalls(calls []openAIToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for i, c := range calls {
		out = append(out, map[string]any{
			"index": i,
			"id":    c.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.Arguments,
			},
		})
	}
	return out
}
