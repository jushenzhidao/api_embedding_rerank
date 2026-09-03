package rerank

import (
	"encoding/json"
	"testing"
)

// 测试 Jina -> Qwen 转换。
func TestJinaToQwen(t *testing.T) {
	topN := 2
	j := &JinaRerankRequest{
		Model:     "qwen3.7-text-rerank",
		Query:     "什么是文本排序模型",
		Documents: []string{"a", "b", "c"},
		TopN:      &topN,
	}
	q := jinaToQwen(j, "")

	if q.Model != "qwen3.7-text-rerank" {
		t.Errorf("model mismatch: %s", q.Model)
	}
	if q.Input.Query != "什么是文本排序模型" {
		t.Errorf("query mismatch: %s", q.Input.Query)
	}
	if len(q.Input.Documents) != 3 {
		t.Errorf("documents length mismatch: %d", len(q.Input.Documents))
	}
	if q.Parameters.TopN != 2 {
		t.Errorf("top_n mismatch: %d", q.Parameters.TopN)
	}
	if q.Parameters.Instruction != "" {
		t.Errorf("instruction should be empty, got %q", q.Parameters.Instruction)
	}
}

// 测试 instruction 透传。
func TestJinaToQwenInstruction(t *testing.T) {
	j := &JinaRerankRequest{
		Model:     "qwen3.7-text-rerank",
		Query:     "q",
		Documents: []string{"a", "b"},
	}
	q := jinaToQwen(j, "Given a web search query, retrieve relevant passages")
	if q.Parameters.Instruction != "Given a web search query, retrieve relevant passages" {
		t.Errorf("instruction mismatch: %q", q.Parameters.Instruction)
	}
}

// 测试 top_n 截断：超过文档数时截断为文档数。
func TestJinaToQwenTopNClamp(t *testing.T) {
	big := 100
	j := &JinaRerankRequest{
		Model:     "m",
		Query:     "q",
		Documents: []string{"a", "b", "c"},
		TopN:      &big,
	}
	q := jinaToQwen(j, "")
	if q.Parameters.TopN != 3 {
		t.Errorf("top_n should be clamped to 3, got %d", q.Parameters.TopN)
	}
}

// 测试 top_n 为 0 视为未指定。
func TestJinaToQwenTopNZero(t *testing.T) {
	zero := 0
	j := &JinaRerankRequest{
		Model:     "m",
		Query:     "q",
		Documents: []string{"a", "b"},
		TopN:      &zero,
	}
	q := jinaToQwen(j, "")
	if q.Parameters.TopN != 0 {
		t.Errorf("top_n should be 0 when not specified, got %d", q.Parameters.TopN)
	}
}

// 测试 Qwen -> Jina 转换，含 return_documents 回填。
func TestQwenToJina(t *testing.T) {
	q := &QwenRerankResponse{}
	q.Output.Results = []QwenResultItem{
		{Index: 2, RelevanceScore: 0.99},
		{Index: 0, RelevanceScore: 0.85},
	}
	q.Usage.TotalTokens = 100

	req := &JinaRerankRequest{
		Model:           "qwen3.7-text-rerank",
		Documents:       []string{"doc0", "doc1", "doc2"},
		ReturnDocuments: true,
	}

	out := qwenToJina(q, req)

	if len(out.Results) != 2 {
		t.Fatalf("results length mismatch: %d", len(out.Results))
	}
	if out.Results[0].Index != 2 || out.Results[0].RelevanceScore != 0.99 {
		t.Errorf("result[0] mismatch: %+v", out.Results[0])
	}
	if out.Results[0].Document == nil || out.Results[0].Document.Text != "doc2" {
		t.Errorf("document not backfilled: %+v", out.Results[0].Document)
	}
	if out.Usage == nil || out.Usage.TotalTokens != 100 {
		t.Errorf("usage mismatch: %+v", out.Usage)
	}
}

// 验证 Jina 请求 JSON 能正确反序列化（模拟 new-api 发来的请求）。
func TestJinaRequestUnmarshal(t *testing.T) {
	raw := `{
		"model": "gte-rerank-v2",
		"query": "什么是文本排序模型",
		"documents": ["a", "b", "c"],
		"top_n": 5,
		"return_documents": true
	}`
	var req JinaRerankRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req.TopN == nil || *req.TopN != 5 {
		t.Errorf("top_n parse failed: %v", req.TopN)
	}
	if !req.ReturnDocuments {
		t.Errorf("return_documents parse failed")
	}
}
