package client

// Update 服务 SDK 测试(httptest 假 relay):List/Latest/Version/Push/DownloadStream。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newUpdateTestServer(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v4.7.5-test","protocol":1,"component":"relay"}`))
	})
	mux.HandleFunc("GET /api/updates", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"versions":[{"version":"v4.7.5-test","sha256":"ab","size":100,"created_at":1}]}`))
	})
	mux.HandleFunc("GET /api/updates/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"latest":{"version":"v4.7.5-test","sha256":"ab","size":100,"created_at":1}}`))
	})
	mux.HandleFunc("POST /api/updates/apply", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mode := "download"
		if strings.Contains(string(body), "binary_path") {
			mode = "path"
		}
		_, _ = w.Write([]byte(`{"agent_id":"a1","task_id":"t1","version":"v4.7.5-test","sha256":"ab","old_version":"v1","binary_size":100,"message":"ok"}`))
		_ = mode
	})
	mux.HandleFunc("GET /api/updates/download", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("version") != "v4.7.5-test" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("X-Xfer-SHA256", "ab")
		_, _ = w.Write([]byte("binary-bytes"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok"), srv
}

func TestClientVersion(t *testing.T) {
	c, _ := newUpdateTestServer(t)
	v, err := c.Version()
	if err != nil || v.Version != "v4.7.5-test" || v.Protocol != 1 {
		t.Fatalf("Version: %+v err=%v", v, err)
	}
}

func TestUpdateListLatest(t *testing.T) {
	c, _ := newUpdateTestServer(t)
	list, err := c.Updates().List()
	if err != nil || len(list) != 1 || list[0].Version != "v4.7.5-test" {
		t.Fatalf("List: %+v err=%v", list, err)
	}
	latest, err := c.Updates().Latest()
	if err != nil || latest == nil || latest.Version != "v4.7.5-test" {
		t.Fatalf("Latest: %+v err=%v", latest, err)
	}
}

func TestUpdatePushModes(t *testing.T) {
	c, _ := newUpdateTestServer(t)
	res, err := c.Updates().Push("a1", "v4.7.5-test", "")
	if err != nil || res.TaskID != "t1" || res.OldVersion != "v1" {
		t.Fatalf("Push(download): %+v err=%v", res, err)
	}
	res2, err := c.Updates().Push("a1", "v4.7.5-test", `C:\ws\x.exe`)
	if err != nil || res2.Version != "v4.7.5-test" {
		t.Fatalf("Push(path): %+v err=%v", res2, err)
	}
}

func TestUpdateDownloadStream(t *testing.T) {
	c, _ := newUpdateTestServer(t)
	body, sha, err := c.Updates().DownloadStream("v4.7.5-test")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	b, _ := io.ReadAll(body)
	if string(b) != "binary-bytes" || sha != "ab" {
		t.Fatalf("Download: %q sha=%q", b, sha)
	}
	// 未知版本 404
	if _, _, err := c.Updates().DownloadStream("nosuch"); err == nil {
		t.Error("未知版本应报错")
	}
}
