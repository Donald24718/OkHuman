package llm

import "testing"

// usageOf 是两条响应路径（非流式 / SSE 尾块）共用的折叠点。这里的优先级是刻意的：
// 优先 OpenAI 的 usage.prompt_tokens_details.cached_tokens，没有才退 llama.cpp 的
// timings.cache_n。归并前两处各写一遍，改一边就只改一边。
func TestUsageOfPrefersOpenAICachedTokens(t *testing.T) {
	u := &wireUsage{PromptTokens: 100, CompletionTokens: 20,
		PromptTokensDetails: &usageDetails{CachedTokens: 77}}
	tm := &wireTimings{CacheN: 42}
	if got := usageOf(u, tm); got.CachedTokens != 77 {
		t.Errorf("两处都给时应取 OpenAI 的 cached_tokens，得 %d", got.CachedTokens)
	}
	u2 := &wireUsage{PromptTokens: 100, CompletionTokens: 20}
	if got := usageOf(u2, tm); got.CachedTokens != 42 {
		t.Errorf("没有 prompt_tokens_details 时应退 llama.cpp timings.cache_n，得 %d", got.CachedTokens)
	}
	if got := usageOf(u2, nil); got.CachedTokens != 0 {
		t.Errorf("两处都没有应为 0，得 %d", got.CachedTokens)
	}
	if got := usageOf(u, nil); got.PromptTokens != 100 || got.CompletionTokens != 20 {
		t.Errorf("token 数照搬： %+v", got)
	}
}
