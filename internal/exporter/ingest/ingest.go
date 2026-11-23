package ingest

// 本文件实现动态化改造 Task 2 的 HTTP 接收入口：POST /api/v1/ingest/build。
//
// 职责编排（校验和存储逻辑分别在 build.go / buildstore.go）：
//  1. 方法检查：只接受 POST，其余 405 + Allow
//  2. 请求体大小限制：防止恶意/异常大包打满内存（默认 1MB，config 可配）
//  3. model.ParseBuildResult：JSON 语法/类型 + 业务校验，失败 400
//  4. BuildStore.Save：新旧判定，重复/旧的 Build 不写指标（幂等，Counter 不重复累加）
//  5. 被接受则立即 BuildMetrics.Update（不等刷新周期——推送是事件驱动，见架构文档）
//
// 为什么不套用 refresher 的 ticker 模式：那是为文件来源（每轮全量重读）设计的，
// 推送来源收到即处理最简单直接；GaugeVec/CounterVec 并发安全已由 updater.go 背书。

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"ops-exporter/internal/exporter/config"
	"ops-exporter/internal/exporter/metrics"
	"ops-exporter/internal/exporter/model"
	"ops-exporter/internal/exporter/store"
)

// IngestHandler 处理 Build 推送请求。
// 依赖通过字段注入，测试可传独立 store/metrics 而不用起真实服务。
type IngestHandler struct {
	store        *store.BuildStore
	metrics      *metrics.BuildMetrics
	maxBodyBytes int64
	logger       *slog.Logger
}

// NewIngestHandler 创建 handler。maxBodyBytes<=0 时回退到默认值。
func NewIngestHandler(store *store.BuildStore, metrics *metrics.BuildMetrics, maxBodyBytes int64, logger *slog.Logger) *IngestHandler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}
	return &IngestHandler{
		store:        store,
		metrics:      metrics,
		maxBodyBytes: maxBodyBytes,
		logger:       logger,
	}
}

// ingestResponse 是成功后返回的统一 JSON 结构。
// accepted=false 表示数据合法但被去重规则忽略（重复提交/旧 Build），
// 这不是错误——Collector 重试不应因此判为发送失败并重投。
type ingestResponse struct {
	Status  string `json:"status"`           // "accepted" 或 "ignored"
	Reason  string `json:"reason,omitempty"` // ignored 时的原因
	JobName string `json:"job_name"`
	Build   int64  `json:"build_number"`
}

// ServeHTTP 实现 http.Handler。
func (h *IngestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeIngestError(w, http.StatusMethodNotAllowed, "method not allowed: use POST")
		return
	}

	// 限制请求体：MaxBytesReader 在超过上限时让 ReadAll 返回错误，
	// 避免一条超大 POST 把内存打满（Task 5 的可靠性要求前置实现）。
	limited := http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	data, err := io.ReadAll(limited)
	if err != nil {
		writeIngestError(w, http.StatusBadRequest, fmt.Sprintf("failed to read request body: %v", err))
		return
	}

	b, err := model.ParseBuildResult(data)
	if err != nil {
		writeIngestError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !h.store.Save(b) {
		// 重复或过时：幂等成功，不写指标，避免 Counter 重复累加。
		resp := ingestResponse{Status: "ignored", Reason: "duplicate or stale build_number", JobName: b.JobName, Build: b.BuildNumber}
		writeIngestJSON(w, http.StatusOK, resp)
		return
	}

	// 被接受：立即写指标（收到即更新，不等刷新周期）。
	h.metrics.Update(b)
	h.logger.Info("build ingested",
		"job", b.JobName,
		"build_number", b.BuildNumber,
		"status", b.Status,
		"duration", b.Duration,
	)

	resp := ingestResponse{Status: "accepted", JobName: b.JobName, Build: b.BuildNumber}
	writeIngestJSON(w, http.StatusOK, resp)
}

// writeIngestJSON 统一 JSON 成功响应。
func writeIngestJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// 编码失败只可能是 payload 含不可编码类型（这里都是基本类型 string/int64），
	// 且响应头已写出无法改为 500；同 handleHealthz 的约定明确忽略写错误。
	_ = json.NewEncoder(w).Encode(payload)
}

// writeIngestError 统一错误响应：JSON 体 + 明确状态码。
func writeIngestError(w http.ResponseWriter, code int, message string) {
	writeIngestJSON(w, code, map[string]string{"error": message})
}
