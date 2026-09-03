// 简单的 mock 上游，模拟 Qwen text-rerank 服务，用于本地端到端测试。
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

type qwenResp struct {
	Output struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	} `json:"output"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
}

func main() {
	http.HandleFunc("/api/v1/services/rerank/text-rerank/text-rerank", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		log.Printf("mock upstream got: %+v", req)

		var resp qwenResp
		resp.Output.Results = []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}{
			{Index: 2, RelevanceScore: 0.98},
			{Index: 0, RelevanceScore: 0.87},
		}
		resp.Usage.TotalTokens = 100
		resp.RequestID = "mock-req-id"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	log.Println("mock qwen upstream listening on :9090")
	log.Fatal(http.ListenAndServe(":9090", nil))
}
