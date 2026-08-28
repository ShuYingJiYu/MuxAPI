// 模型名归一化：转发前把客户端模型名转换成上游真实支持的名称。
// 两类处理：
//  1. 剥离上下文窗口后缀（如 stealth/ox-alpha[1M] 的 [1M]）。客户端用后缀声明
//     长上下文意图，但聚合上游（OpenRouter 等）把它当模型名的一部分直接报
//     400；上下文长度由上游自身能力决定，剥离无损。
//  2. 别名映射（MUXAPI_MODEL_ALIASES，"from=to" 逗号分隔）。把历史名或别名
//     重写到组内真实模型，等价于 newapi model_mapping 的最小集。
package forward

import (
	"strconv"
	"strings"
)

// stripContextSuffix 剥离结尾的上下文窗口后缀（[1M]、[200k] 等）。
// 非数字+K/M 形态的方括号后缀不是上下文声明，原样保留。
func stripContextSuffix(model string) string {
	if !strings.HasSuffix(model, "]") {
		return model
	}
	idx := strings.LastIndex(model, "[")
	if idx <= 0 {
		return model
	}
	inner := model[idx+1 : len(model)-1]
	if len(inner) < 2 {
		return model
	}
	switch inner[len(inner)-1] {
	case 'K', 'k', 'M', 'm':
	default:
		return model
	}
	if _, err := strconv.Atoi(inner[:len(inner)-1]); err != nil {
		return model
	}
	return model[:idx]
}

// canonicalizeModel 返回归一化后的模型名：先剥上下文后缀，再查别名表。
func (f *Forwarder) canonicalizeModel(model string) string {
	if model == "" {
		return model
	}
	stripped := stripContextSuffix(model)
	if mapped, ok := f.modelAliases[stripped]; ok && mapped != "" {
		return mapped
	}
	return stripped
}

// parseModelAliases 解析 "from=to" 逗号分隔的别名配置；空段和缺 to 的段忽略。
// from 同样先剥上下文后缀，保证 claude-x[1M]=y 这类写法也能命中。
func ParseModelAliases(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	aliases := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(pair), "=")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" {
			continue
		}
		aliases[stripContextSuffix(from)] = to
	}
	if len(aliases) == 0 {
		return nil
	}
	return aliases
}
