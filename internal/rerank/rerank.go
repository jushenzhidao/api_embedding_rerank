// Package rerank 实现 Jina rerank 格式与 Qwen (阿里云百炼 DashScope)
// text-rerank 服务之间的协议转换，并以 HTTP 服务形式对外提供。
//
// 设计要点：
//   - 对外暴露 Jina 兼容的 POST /v1/rerank 端点，供 new-api / One API 直接对接。
//   - base_url（即 Qwen 的 WorkspaceId 前缀）通过请求头传入，实现多 Workspace 复用。
//   - 高并发安全：单一 http.Client 连接池 + 信号量限流 + sync.Pool 缓冲复用 + 超时控制。
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 默认配置项（可通过环境变量覆盖，见 Config）。
const (
	defaultTimeout        = 60 * time.Second
	defaultMaxConcurrency = 200 // 上游并发上限，防止打爆百炼账号限流
	defaultPoolSize       = 200 // 连接池空闲连接数，匹配并发上限
)

// Config 服务运行配置。
type Config struct {
	// Timeout 上游请求整体超时（含建连、等待响应、读 body）。
	Timeout time.Duration
	// MaxConcurrency 同时向上游发起的最大请求数，超出部分排队等待。
	MaxConcurrency int
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Timeout:        defaultTimeout,
		MaxConcurrency: defaultMaxConcurrency,
	}
}

// Service 负责协议转换与上游调用。所有字段在构造后只读，天然并发安全。
type Service struct {
	cfg Config

	// client 是全局唯一的 HTTP 客户端，复用底层 TCP 连接池，避免高并发下
	// 每次请求都重建连接导致 TIME_WAIT 堆积与握手开销。
	client *http.Client

	// sem 是并发信号量，限制同时打向上游的请求数，实现背压与削峰。
	sem chan struct{}

	// bufPool 复用请求/响应字节缓冲，降低高并发下的 GC 压力。
	bufPool sync.Pool
}

// New 构造 Service。
func New(cfg Config) *Service {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}

	transport := &http.Transport{
		MaxIdleConns:        cfg.MaxConcurrency,
		MaxIdleConnsPerHost: cfg.MaxConcurrency, // 关键：针对单一上游 host 复用连接
		IdleConnTimeout:     90 * time.Second,
		// DisableCompression 保持 false，允许 gzip 减少传输量。
	}

	return &Service{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
			// 禁止自动跟随重定向，避免 token 被带往非预期 host。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		sem: make(chan struct{}, cfg.MaxConcurrency),
		bufPool: sync.Pool{
			New: func() any { return bytes.NewBuffer(make([]byte, 0, 4096)) },
		},
	}
}

// ---------------------------------------------------------------------------
// Jina 兼容协议结构（对外接口）
// ---------------------------------------------------------------------------

// JinaRerankRequest 是 Jina / Cohere 风格的 rerank 请求体。
// Instruction 为可选扩展字段（对应百炼 text-rerank 的 instruction 参数），
// 也可通过请求头 X-Instruct 传入，优先级：请求体 < 请求头。
type JinaRerankRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            *int     `json:"top_n,omitempty"`
	ReturnDocuments bool     `json:"return_documents,omitempty"`
	Instruction     string   `json:"instruction,omitempty"`
}

// JinaRerankResponse 是 Jina 风格的 rerank 响应体。
type JinaRerankResponse struct {
	ID      string           `json:"id,omitempty"`
	Model   string           `json:"model"`
	Results []JinaResultItem `json:"results"`
	Usage   *Usage           `json:"usage,omitempty"`
}

// JinaResultItem 单条排序结果。
type JinaResultItem struct {
	Index          int         `json:"index"`
	Document       *DocObject  `json:"document,omitempty"`
	RelevanceScore float64     `json:"relevance_score"`
	Meta           interface{} `json:"meta,omitempty"`
}

// DocObject return_documents 为 true 时回填的文档对象。
type DocObject struct {
	Text string `json:"text"`
}

// Usage 简化的用量信息（可选）。
type Usage struct {
	TotalTokens int `json:"total_tokens,omitempty"`
}

// ---------------------------------------------------------------------------
// Qwen 百炼协议结构（上游接口）
// ---------------------------------------------------------------------------

// QwenRerankRequest 百炼 text-rerank 请求体。
type QwenRerankRequest struct {
	Model      string     `json:"model"`
	Input      QwenInput  `json:"input"`
	Parameters QwenParams `json:"parameters,omitempty"`
}

// QwenInput 百炼输入。
type QwenInput struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

// QwenParams 百炼参数。instruction 为可选字段。
type QwenParams struct {
	TopN        int    `json:"top_n,omitempty"`
	Instruction string `json:"instruction,omitempty"`
}

// QwenRerankResponse 百炼 text-rerank 响应体。
type QwenRerankResponse struct {
	Output struct {
		Results []QwenResultItem `json:"results"`
	} `json:"output"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	// 错误时返回的结构
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// QwenResultItem 百炼单条结果。
type QwenResultItem struct {
	Index          int        `json:"index"`
	Document       *DocObject `json:"document,omitempty"`
	RelevanceScore float64    `json:"relevance_score"`
}

// ---------------------------------------------------------------------------
// 转换逻辑
// ---------------------------------------------------------------------------

// jinaToQwen 将 Jina 请求转换为百炼请求。
// instruct 为最终生效的 instruction（已合并请求体与请求头，可空）。
func jinaToQwen(j *JinaRerankRequest, instruct string) *QwenRerankRequest {
	q := &QwenRerankRequest{
		Model: j.Model,
		Input: QwenInput{
			Query:     j.Query,
			Documents: j.Documents,
		},
	}
	if j.TopN != nil && *j.TopN > 0 {
		// top_n 超过文档数时截断，避免上游报错或返回越界。
		if n := len(j.Documents); *j.TopN > n {
			q.Parameters.TopN = n
		} else {
			q.Parameters.TopN = *j.TopN
		}
	}
	if instruct != "" {
		q.Parameters.Instruction = instruct
	}
	return q
}

// qwenToJina 将百炼响应转换为 Jina 响应。
func qwenToJina(q *QwenRerankResponse, req *JinaRerankRequest) *JinaRerankResponse {
	out := &JinaRerankResponse{
		Model:   req.Model,
		Results: make([]JinaResultItem, 0, len(q.Output.Results)),
	}

	for _, r := range q.Output.Results {
		item := JinaResultItem{
			Index:          r.Index,
			RelevanceScore: r.RelevanceScore,
		}
		if req.ReturnDocuments && r.Index >= 0 && r.Index < len(req.Documents) {
			item.Document = &DocObject{Text: req.Documents[r.Index]}
		} else if r.Document != nil {
			item.Document = r.Document
		}
		out.Results = append(out.Results, item)
	}

	if q.Usage.TotalTokens > 0 {
		out.Usage = &Usage{TotalTokens: q.Usage.TotalTokens}
	}
	return out
}

// ---------------------------------------------------------------------------
// 请求处理
// ---------------------------------------------------------------------------

// 请求头常量。
const (
	headerBaseURL     = "X-Base-Url"     // 自定义：上游 base_url，如 https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com
	headerUpstreamKey = "X-Upstream-Key" // 自定义：百炼 API Key（可覆盖 Authorization）
	headerInstruct    = "X-Instruct"     // 自定义：百炼 rerank 的 instruction 参数（可覆盖请求体）
	headerAuth        = "Authorization"  // 标准：Bearer 认证
)

// 上游路径后缀（相对 base_url）。
const rerankPath = "/api/v1/services/rerank/text-rerank/text-rerank"

// maxBodyBytes 限制请求体大小，防止恶意大请求耗尽内存。
const maxBodyBytes = 10 << 20 // 10 MB

// ServeHTTP 实现 http.Handler，处理 Jina -> Qwen 的转换。
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 只接受 POST /v1/rerank（也容忍 /rerank 路径以兼容 new-api 配置）。
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path != "/v1/rerank" && path != "/rerank" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	// 1. 读取并限制请求体。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	if len(body) > maxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}

	// 2. 解析 Jina 请求。
	var jreq JinaRerankRequest
	if err := json.Unmarshal(body, &jreq); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if jreq.Query == "" || len(jreq.Documents) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query and documents are required"})
		return
	}

	// 3. 提取 base_url、key 与 instruction。
	baseURL := strings.TrimSpace(r.Header.Get(headerBaseURL))
	apiKey := ""
	if k := strings.TrimSpace(r.Header.Get(headerUpstreamKey)); k != "" {
		apiKey = k
	} else {
		apiKey = strings.TrimSpace(strings.TrimPrefix(r.Header.Get(headerAuth), "Bearer "))
	}
	if baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing " + headerBaseURL + " header"})
		return
	}
	if apiKey == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing upstream api key"})
		return
	}
	// instruction 优先级：请求头 X-Instruct > 请求体 instruction 字段。
	instruct := strings.TrimSpace(r.Header.Get(headerInstruct))
	if instruct == "" {
		instruct = strings.TrimSpace(jreq.Instruction)
	}

	// 4. 调用上游并转换（传入请求 context，支持客户端断开时取消排队）。
	jresp, status, upstreamErr := s.callUpstream(r.Context(), baseURL, apiKey, &jreq, instruct)
	if upstreamErr != nil {
		writeJSON(w, status, map[string]string{"error": upstreamErr.Error()})
		return
	}

	// 5. 返回结果。
	writeJSON(w, http.StatusOK, jresp)
}

// callUpstream 发起百炼请求并返回转换后的 Jina 响应。
// 返回 (jinaResp, httpStatus, err)。err 非空时 status 为对应错误码。
func (s *Service) callUpstream(ctx context.Context, baseURL, apiKey string, jreq *JinaRerankRequest, instruct string) (*JinaRerankResponse, int, error) {
	qreq := jinaToQwen(jreq, instruct)

	// 序列化上游请求体，复用缓冲降低 GC。
	buf := s.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer s.bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(qreq); err != nil {
		return nil, http.StatusInternalServerError, errors.New("encode upstream request: " + err.Error())
	}

	url := strings.TrimSuffix(baseURL, "/") + rerankPath
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, http.StatusInternalServerError, errors.New("build upstream request: " + err.Error())
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+apiKey)

	// 并发限流：信号量排队，客户端断开时也能取消等待。
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, http.StatusRequestTimeout, errors.New("request cancelled while waiting for upstream slot")
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		return nil, http.StatusBadGateway, errors.New("upstream error: " + err.Error())
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, http.StatusBadGateway, errors.New("read upstream response: " + err.Error())
	}

	// 上游非 200：透传错误信息。
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = "upstream status " + strconv.Itoa(resp.StatusCode)
		}
		return nil, resp.StatusCode, errors.New(msg)
	}

	var qresp QwenRerankResponse
	if err := json.Unmarshal(body, &qresp); err != nil {
		return nil, http.StatusBadGateway, errors.New("decode upstream response: " + err.Error())
	}
	// 百炼业务错误（HTTP 200 但 code 非空）。
	if qresp.Code != "" && qresp.Code != "OK" {
		return nil, http.StatusBadGateway, errors.New("qwen error: " + qresp.Code + " " + qresp.Message)
	}

	return qwenToJina(&qresp, jreq), http.StatusOK, nil
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

// writeJSON 写入 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler 返回服务的 http.Handler。
func (s *Service) Handler() http.Handler {
	return s
}
