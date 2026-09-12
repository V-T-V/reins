// Package main — update_repo.go 单元测试(Agent 二进制版本仓库)。
//
// 覆盖:
//   - Save:显式版本 / 自动时间戳版本、SHA256 精确锚点、落盘文件名
//   - List:版本降序、非二进制文件(.sha256/无关文件)排除、Size/SHA256 字段
//   - BinPath:.exe / .agent 双扩展探测、缺失返回空串
//   - readSHA:缺失返回空串
//   - 端点:GET /api/updates(nil repo → enabled=false;有 repo → 列表+count)、
//     POST /api/updates/upload(未启用 503;成功 200 + sha256 回读)、
//     POST /api/updates/apply(参数缺失 400;版本不在仓库 404;Agent 不在线 404)
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateRepoSaveAndSHA(t *testing.T) {
	dir := t.TempDir()
	repo, err := NewUpdateRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := []byte("fake-agent-binary-content-v4.4.0")
	ver, sha, err := repo.Save("v4.4.0", bytes.NewReader(bin))
	if err != nil {
		t.Fatal(err)
	}
	if ver != "v4.4.0" {
		t.Fatalf("version echo: %q", ver)
	}
	want := sha256.Sum256(bin)
	if sha != hex.EncodeToString(want[:]) {
		t.Fatalf("sha mismatch: got %s want %s", sha, hex.EncodeToString(want[:]))
	}
	// 落盘文件:.exe 二进制 + .sha256 旁车文件。
	if _, err := os.Stat(filepath.Join(dir, "agent-binaries", "v4.4.0.exe")); err != nil {
		t.Fatalf("binary not persisted: %v", err)
	}
	shaFile, err := os.ReadFile(filepath.Join(dir, "agent-binaries", "v4.4.0.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(shaFile)) != sha {
		t.Fatalf("sidecar sha: %q want %q", shaFile, sha)
	}
	// BinPath 探测到 .exe。
	if p := repo.BinPath("v4.4.0"); !strings.HasSuffix(p, "v4.4.0.exe") {
		t.Fatalf("BinPath: %q", p)
	}
}

func TestUpdateRepoSaveAutoVersion(t *testing.T) {
	repo, _ := NewUpdateRepo(t.TempDir())
	ver, _, err := repo.Save("", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ver, "v") || len(ver) != len("v20060102-150405") {
		t.Fatalf("auto version format: %q", ver)
	}
}

func TestUpdateRepoList(t *testing.T) {
	dir := t.TempDir()
	repo, _ := NewUpdateRepo(dir)
	abin := []byte("aaa")
	bbin := []byte("bbbb")
	cbin := []byte("ccccc")
	for _, c := range []struct {
		ver string
		bin []byte
	}{{"v1.0.0", abin}, {"v2.0.0", bbin}, {"v10.0.0", cbin}} {
		if _, _, err := repo.Save(c.ver, bytes.NewReader(c.bin)); err != nil {
			t.Fatal(err)
		}
	}
	// 干扰文件:裸 .sha256(无对应二进制)与无关文件,均不得出现在列表。
	os.WriteFile(filepath.Join(dir, "agent-binaries", "ghost.sha256"), []byte("ff"), 0o644)
	os.WriteFile(filepath.Join(dir, "agent-binaries", "README.txt"), []byte("x"), 0o644)

	list := repo.List()
	if len(list) != 3 {
		t.Fatalf("want 3 versions, got %d: %+v", len(list), list)
	}
	// 降序(字典序:v10.0.0 > v2.0.0 > v1.0.0)。
	wantOrder := []string{"v10.0.0", "v2.0.0", "v1.0.0"}
	for i, w := range wantOrder {
		if list[i].Version != w {
			t.Fatalf("order[%d]: got %s want %s (full: %+v)", i, list[i].Version, w, list)
		}
	}
	// 字段锚点:大小与 SHA。
	byVer := map[string]VersionInfo{}
	for _, vi := range list {
		byVer[vi.Version] = vi
	}
	if byVer["v1.0.0"].Size != int64(len(abin)) {
		t.Fatalf("size: %d", byVer["v1.0.0"].Size)
	}
	wantSHA := sha256.Sum256(cbin)
	if byVer["v10.0.0"].SHA256 != hex.EncodeToString(wantSHA[:]) {
		t.Fatalf("sha field mismatch")
	}
	if byVer["v10.0.0"].CreatedAt == 0 {
		t.Fatal("CreatedAt must be nonzero")
	}
}

func TestUpdateRepoBinPathAgentExt(t *testing.T) {
	dir := t.TempDir()
	repo, _ := NewUpdateRepo(dir)
	// 手工放一个 .agent 扩展二进制(Linux 风格)。
	binDir := filepath.Join(dir, "agent-binaries")
	os.WriteFile(filepath.Join(binDir, "v9.9.9.agent"), []byte("elf"), 0o644)
	if p := repo.BinPath("v9.9.9"); !strings.HasSuffix(p, "v9.9.9.agent") {
		t.Fatalf("BinPath .agent: %q", p)
	}
	if p := repo.BinPath("missing"); p != "" {
		t.Fatalf("missing version must return empty, got %q", p)
	}
	if s := repo.readSHA("missing"); s != "" {
		t.Fatalf("missing sha must be empty, got %q", s)
	}
}

// --- HTTP 端点 ---

func TestHandleUpdateListDisabled(t *testing.T) {
	_, srv := newTestRelay(t) // updateRepo 为 nil
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/updates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		Versions []any `json:"versions"`
		Enabled  bool  `json:"enabled"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Enabled || len(body.Versions) != 0 {
		t.Fatalf("disabled repo: %+v", body)
	}
}

func TestHandleUpdateListAndUpload(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	repo, err := NewUpdateRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.updateRepo = repo

	// 上传一个版本。
	bin := []byte("agent-bin-payload")
	resp, err := http.Post(srv.URL+"/api/updates/upload?version=v5.0.0", "application/octet-stream", bytes.NewReader(bin))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: want 200, got %d", resp.StatusCode)
	}
	var up struct {
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
	}
	json.NewDecoder(resp.Body).Decode(&up)
	want := sha256.Sum256(bin)
	if up.Version != "v5.0.0" || up.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("upload body: %+v", up)
	}

	// 列表包含刚上传的版本。
	resp2, err := http.Get(srv.URL + "/api/updates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list struct {
		Versions []VersionInfo `json:"versions"`
		Count    int           `json:"count"`
		Enabled  bool          `json:"enabled"`
	}
	json.NewDecoder(resp2.Body).Decode(&list)
	if !list.Enabled || list.Count != 1 || len(list.Versions) != 1 {
		t.Fatalf("list body: %+v", list)
	}
	if list.Versions[0].Version != "v5.0.0" || list.Versions[0].SHA256 != up.SHA256 {
		t.Fatalf("listed version: %+v", list.Versions[0])
	}
}

func TestHandleUpdateUploadDisabled(t *testing.T) {
	_, srv := newTestRelay(t) // updateRepo nil
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/updates/upload?version=v1", "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

func TestHandleUpdateApplyValidation(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	repo, _ := NewUpdateRepo(t.TempDir())
	r.updateRepo = repo

	post := func(body string) int {
		resp, err := http.Post(srv.URL+"/api/updates/apply", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	// 坏 JSON → 400。
	if c := post("{not json"); c != http.StatusBadRequest {
		t.Fatalf("bad json: want 400, got %d", c)
	}
	// 缺 agent_id → 400。
	if c := post(`{"version":"v1.0.0"}`); c != http.StatusBadRequest {
		t.Fatalf("missing agent_id: want 400, got %d", c)
	}
	// 缺 version → 400。
	if c := post(`{"agent_id":"a1"}`); c != http.StatusBadRequest {
		t.Fatalf("missing version: want 400, got %d", c)
	}
	// 版本不在仓库 → 404。
	if c := post(`{"agent_id":"a1","version":"v0.0.0"}`); c != http.StatusNotFound {
		t.Fatalf("unknown version: want 404, got %d", c)
	}
	// 版本在仓库但 Agent 不在线 → 404。
	repo.Save("v1.0.0", strings.NewReader("bin"))
	if c := post(`{"agent_id":"ghost","version":"v1.0.0"}`); c != http.StatusNotFound {
		t.Fatalf("agent offline: want 404, got %d", c)
	}
}
