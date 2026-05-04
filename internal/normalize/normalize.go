// Package normalize provides helpers for mapping provider-specific wire
// values to bridle's canonical string constants.
package normalize

// ClaudeStopReason maps Claude API stop_reason strings to bridle StopReason values.
func ClaudeStopReason(raw string) string {
	switch raw {
	case "end_turn":
		return "model_done"
	case "max_tokens":
		return "max_steps"
	case "tool_use":
		// tool_use is not terminal in bridle; the caller manages the loop.
		return "model_done"
	default:
		return "model_done"
	}
}

// OpenAIStopReason maps OpenAI finish_reason strings to bridle StopReason values.
func OpenAIStopReason(raw string) string {
	switch raw {
	case "stop":
		return "model_done"
	case "length":
		return "max_steps"
	case "tool_calls", "function_call":
		return "model_done"
	default:
		return "model_done"
	}
}

// GeminiStopReason maps Gemini FinishReason values to bridle StopReason values.
func GeminiStopReason(raw string) string {
	switch raw {
	case "STOP", "FINISH_REASON_STOP":
		return "model_done"
	case "MAX_TOKENS":
		return "max_steps"
	default:
		return "model_done"
	}
}

// OllamaStopReason maps Ollama done_reason strings to bridle StopReason values.
func OllamaStopReason(raw string) string {
	switch raw {
	case "stop":
		return "model_done"
	case "length":
		return "max_steps"
	default:
		return "model_done"
	}
}
