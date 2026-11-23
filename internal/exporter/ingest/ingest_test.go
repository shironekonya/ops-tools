package ingest

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"ops-exporter/internal/exporter/metrics"
	"ops-exporter/internal/exporter/store"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// newTestIngest 创建一套独立的 handler + store + registry，测试之间互不干扰。
func newTestIngest(t *testing.T, maxBody int64) (*IngestHandler, *store.BuildStore, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	bm, err := metrics.NewBuildMetrics(reg)
	if err != nil {
		t.Fatalf("metrics.NewBuildMetrics: %v", err)
	}
	store := &store.BuildStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewIngestHandler(store, bm, maxBody, logger)
	return h, store, reg
}

func postBuild(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/build", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestIngest_Valid 正常 JSON：200 + accepted，数据进 store、指标被写入，不等刷新周期。
func TestIngest_Valid(t *testing.T) {
	h, store, reg := newTestIngest(t, 0)
	w := postBuild(t, h, validBuildJSON())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ingestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("status = %q, want accepted", resp.Status)
	}
	if b, ok := store.Get("my-pipeline"); !ok || b.BuildNumber != 42 {
		t.Errorf("store not updated: %+v ok=%v", b, ok)
	}
	// 收到即写指标（Evidence：不等 ticker，这里直接 gather 就能看到值）
	if got := metricValue(t, reg, "jenkins_build_status", "my-pipeline"); got != 0 {
		t.Errorf("jenkins_build_status = %v, want 0 (failed)", got)
	}
	if got := metricValue(t, reg, "jenkins_build_total", "my-pipeline"); got != 1 {
		t.Errorf("jenkins_build_total = %v, want 1", got)
	}
	if got := metricValue(t, reg, "jenkins_build_failure_total", "my-pipeline"); got != 1 {
		t.Errorf("jenkins_build_failure_total = %v, want 1", got)
	}
}

// TestIngest_InvalidBodies 错误 JSON / 缺少必填字段 / 非法状态都返回 400，且不写指标。
func TestIngest_InvalidBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"语法错误", `{"job_name": "x", `},
		{"完全不是 JSON", `not a json`},
		{"缺少 job_name", `{"build_number":1,"status":"success","duration":1,"timestamp":1700000000}`},
		{"状态非法", `{"job_name":"x","build_number":1,"status":"running","duration":1,"timestamp":1700000000}`},
		{"build_number 为零", `{"job_name":"x","build_number":0,"status":"success","duration":1,"timestamp":1700000000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, reg := newTestIngest(t, 0)
			w := postBuild(t, h, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			// 错误响应必须是 JSON 且含 error 字段（错误响应规范）
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if resp["error"] == "" {
				t.Errorf("error response missing 'error' field: %s", w.Body.String())
			}
			// 非法数据不能污染指标
			families, err := reg.Gather()
			if err != nil {
				t.Fatalf("gather: %v", err)
			}
			for _, f := range families {
				if strings.HasPrefix(f.GetName(), "jenkins_build_") && len(f.GetMetric()) > 0 {
					t.Errorf("invalid body produced metric %s", f.GetName())
				}
			}
		})
	}
}

// TestIngest_Duplicate 重复提交：第二次 ignored，Counter 不重复累加（幂等）。
func TestIngest_Duplicate(t *testing.T) {
	h, _, reg := newTestIngest(t, 0)
	first := postBuild(t, h, validBuildJSON())
	second := postBuild(t, h, validBuildJSON())

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("status first=%d second=%d, want both 200", first.Code, second.Code)
	}
	var resp ingestResponse
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ignored" || resp.Reason == "" {
		t.Errorf("duplicate response = %+v, want ignored with reason", resp)
	}
	if got := metricValue(t, reg, "jenkins_build_total", "my-pipeline"); got != 1 {
		t.Errorf("total after duplicate = %v, want 1 (no double count)", got)
	}
}

// TestIngest_StaleBuild 旧 build_number 乱序到达：ignored，不覆盖新状态。
func TestIngest_StaleBuild(t *testing.T) {
	h, store, reg := newTestIngest(t, 0)
	newer := strings.Replace(validBuildJSON(), `"build_number": 42`, `"build_number": 10`, 1)
	newer = strings.Replace(newer, `"status": "failed"`, `"status": "success"`, 1)
	postBuild(t, h, newer)

	older := strings.Replace(validBuildJSON(), `"build_number": 42`, `"build_number": 9`, 1)
	w := postBuild(t, h, older)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp ingestResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "ignored" {
		t.Errorf("stale response status = %q, want ignored", resp.Status)
	}
	if b, _ := store.Get("my-pipeline"); b.BuildNumber != 10 || b.Status != BuildStatusSuccess {
		t.Errorf("stale build overwrote state: %+v", b)
	}
	if got := metricValue(t, reg, "jenkins_build_status", "my-pipeline"); got != 1 {
		t.Errorf("status = %v, want 1 (newer success preserved)", got)
	}
}

// TestIngest_MethodNotAllowed GET 返回 405 且带 Allow 头。
func TestIngest_MethodNotAllowed(t *testing.T) {
	h, _, _ := newTestIngest(t, 0)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ingest/build", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}
}

// TestIngest_BodyTooLarge 超过 max_body_bytes 返回 400，不写指标。
func TestIngest_BodyTooLarge(t *testing.T) {
	h, _, reg := newTestIngest(t, 64) // 64 字节，合法 JSON 远大于此
	w := postBuild(t, h, validBuildJSON())
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "jenkins_build_") && len(f.GetMetric()) > 0 {
			t.Errorf("oversized body produced metric %s", f.GetName())
		}
	}
}

// TestIngest_Concurrent 并发提交：验证三件事（配合 -count 重复运行）：
//  1. 无 panic、无死锁（用到 Mutex 与 Gauge/Counter 内部锁）；
//  2. store 收敛到最大 build_number（乱序到达时旧编号被拒）；
//  3. total 计数 == 实际被接纳的次数（accepted 响应数），重复/旧的不会重复累加。
func TestIngest_Concurrent(t *testing.T) {
	h, store, reg := newTestIngest(t, 0)
	const n = 50

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(num int64) {
			defer wg.Done()
			body := strings.Replace(validBuildJSON(), `"build_number": 42`, `"build_number": `+strconv.FormatInt(num, 10), 1)
			w := postBuild(t, h, body)
			if w.Code != http.StatusOK {
				t.Errorf("request %d: status = %d, want 200", num, w.Code)
				return
			}
			var resp ingestResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Errorf("request %d: decode: %v", num, err)
				return
			}
			if resp.Status == "accepted" {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(int64(i))
	}
	wg.Wait()

	if b, ok := store.Get("my-pipeline"); !ok || b.BuildNumber != n {
		t.Errorf("final store = %+v ok=%v, want build_number=%d", b, ok, n)
	}
	if got := metricValue(t, reg, "jenkins_build_total", "my-pipeline"); got != float64(accepted) {
		t.Errorf("total = %v, want %d (number of accepted responses)", got, accepted)
	}
	if accepted < 1 || accepted > n {
		t.Errorf("accepted = %d, want in [1,%d]", accepted, n)
	}
}
