package chatcompletions

// The summary carrier survives Responses clients that intentionally omit raw
// reasoning content on replay. This marker distinguishes full upstream text
// from foreign reasoning summaries; no encrypted payload is decoded or forged.
const reasoningIDPrefix = "rs_cpa_chat_"

func reasoningItem(id, text string) map[string]any {
	return map[string]any{"type": "reasoning", "id": reasoningIDPrefix + id,
		"summary": []any{map[string]any{"type": "summary_text", "text": text}}}
}
