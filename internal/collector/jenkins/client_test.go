package jenkins

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGetLatestBuild 验证结构化信息解析与 Basic Auth 头。
func TestGetLatestBuild(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/job/deploy/lastBuild/api/json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42,"result":"FAILURE","timestamp":1700000000000,"duration":123500}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "admin", "tok123", time.Second)
	info, ok, err := c.GetLatestBuild("deploy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok")
	}
	if info.Number != 42 || info.Result != "FAILURE" || info.TimestampMs != 1700000000000 || info.DurationMs != 123500 {
		t.Errorf("info = %+v", info)
	}
	if info.JobName != "deploy" {
		t.Errorf("JobName = %q", info.JobName)
	}

	// Basic Auth: base64("admin:tok123")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:tok123"))
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestGetLatestBuild_Building result=null 时 ok=false。
func TestGetLatestBuild_Building(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":43,"result":null,"timestamp":1700000000000,"duration":0}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "", time.Second)
	_, ok, err := c.GetLatestBuild("deploy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("building build (result=null) should return ok=false")
	}
}

// TestGetConsoleLog 取回原始日志文本。
func TestGetConsoleLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/job/deploy/42/consoleText" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("[Pipeline] Stage: Deploy\nERROR: connection refused\nFinished: FAILURE\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "", time.Second)
	log, err := c.GetConsoleLog("deploy", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(log, "ERROR: connection refused") {
		t.Errorf("log missing content: %q", log)
	}
}

// TestGetConsoleLog_InvalidNumber 非法 build number 直接报错，不发请求。
func TestGetConsoleLog_InvalidNumber(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "", "", time.Second)
	if _, err := c.GetConsoleLog("deploy", 0); err == nil {
		t.Fatal("expected error for number=0")
	}
}

// TestClient_HTTPErrors 非 2xx 状态码返回错误。
func TestClient_HTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "job not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "", time.Second)
	if _, _, err := c.GetLatestBuild("missing"); err == nil {
		t.Fatal("expected error on 404")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status: %v", err)
	}
}

// TestClient_Timeout 请求超时返回错误（验证超时被注入到 http.Client）。
func TestClient_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "", 50*time.Millisecond)
	if _, _, err := c.GetLatestBuild("slow"); err == nil {
		t.Fatal("expected timeout error")
	}
}

// TestClient_JobNameEscaped Job 名含空格/斜杠时做路径转义，不破坏 URL。
func TestClient_JobNameEscaped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"number":1,"result":"SUCCESS","timestamp":1,"duration":1}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "", time.Second)
	if _, _, err := c.GetLatestBuild("folder/job name"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotPath, "folder%2Fjob%20name") {
		t.Errorf("path not escaped: %q", gotPath)
	}
}
