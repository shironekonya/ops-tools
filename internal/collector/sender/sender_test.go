package sender

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ops-exporter/internal/collector/model"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sampleBuild() model.BuildResult {
	return model.BuildResult{
		JobName: "deploy", BuildNumber: 42, Status: model.StatusFailed,
		Duration: 1.5, Timestamp: 1700000000, FailedStage: "deploy",
	}
}

// TestSender_Success 成功发送：POST 方法、Content-Type、体内容正确。
func TestSender_Success(t *testing.T) {
	var gotMethod, gotCT string
	var gotBuild model.BuildResult
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBuild)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer srv.Close()

	s := NewSender(srv.URL, time.Second, 0, quietLogger())
	if err := s.Send(sampleBuild()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost || gotCT != "application/json" {
		t.Errorf("method=%q ct=%q", gotMethod, gotCT)
	}
	if gotBuild.JobName != "deploy" || gotBuild.BuildNumber != 42 || gotBuild.Status != "failed" {
		t.Errorf("body wrong: %+v", gotBuild)
	}
}

// TestSender_IgnoredIsSuccess Exporter 返回 200 ignored（重复 build）视为成功。
func TestSender_IgnoredIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored","reason":"duplicate"}`))
	}))
	defer srv.Close()

	s := NewSender(srv.URL, time.Second, 0, quietLogger())
	if err := s.Send(sampleBuild()); err != nil {
		t.Fatalf("ignored should be success, got: %v", err)
	}
}

// TestSender_4xxNoRetry 4xx（数据非法）不重试，只请求一次。
func TestSender_4xxNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	s := NewSender(srv.URL, time.Second, 3, quietLogger())
	if err := s.Send(sampleBuild()); err == nil {
		t.Fatal("expected error on 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", got)
	}
}

// TestSender_5xxRetry 5xx 重试，最终成功。
func TestSender_5xxRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewSender(srv.URL, time.Second, 3, quietLogger())
	if err := s.Send(sampleBuild()); err != nil {
		t.Fatalf("should succeed after retries: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

// TestSender_RetryExhausted 重试耗尽返回最后一次错误，请求次数 = maxRetries+1。
func TestSender_RetryExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewSender(srv.URL, time.Second, 2, quietLogger())
	if err := s.Send(sampleBuild()); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3 (1+2 retries)", got)
	}
}

// TestSender_Timeout 请求超时经重试后返回错误（验证 timeout 注入）。
func TestSender_Timeout(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewSender(srv.URL, 50*time.Millisecond, 1, quietLogger())
	if err := s.Send(sampleBuild()); err == nil {
		t.Fatal("expected timeout error")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2 (1+1 retry)", got)
	}
}
