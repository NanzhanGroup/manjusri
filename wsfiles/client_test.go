package wsfiles

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeFiles 一个最小可用的 ws-files 假服务：单发 + init/chunk/complete/abort + delete。
type fakeFiles struct {
	mu        sync.Mutex
	url       string // httptest 站点地址
	infoChunk int64
	// 强制失败次数（按 kind 计数），用于验证重试/故障转移
	failSingle int
	failChunk  int
	singleHits int
	initHits   int
	chunkHits  int
	aborts     int
	deleted    []string
	knownIDs   map[string]bool   // 该站「确实存在」的文件 id（其余返回 deleted=false）
	gotSHA     map[string]string // upload_id → 声明的 sha256
	chunksRecv map[string]map[int]string

	// 鉴权口径：默认按 wsauth v1 校验**节点签名**（与 ws-files 0.5.0+ 一致）
	requireSig bool // true ⇒ 按协议验签，签名不对即 401
	pub        ed25519.PublicKey
	seenTokens []string // 每个请求实际带到的令牌头（断言"签名模式不发令牌"）
	seenNonces []string // 每个请求的 nonce（断言"重试必须重签"）
	seenCerts  []string // 每个请求携带的证书
	badSigs    int      // 验签失败次数
}

func newFake() *fakeFiles {
	return &fakeFiles{
		infoChunk:  400000,
		knownIDs:   map[string]bool{},
		gotSHA:     map[string]string{},
		chunksRecv: map[string]map[int]string{},
	}
}

func (f *fakeFiles) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJ(w, 200, map[string]any{"ok": true, "chunk_size": f.infoChunk, "single_shot_max_raw": SingleShotRawMax})
	})
	mux.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.singleHits++
		if f.failSingle > 0 {
			f.failSingle--
			f.mu.Unlock()
			writeJ(w, 500, map[string]any{"ok": false, "error": "boom"})
			return
		}
		f.mu.Unlock()
		mr, err := r.MultipartReader()
		if err != nil {
			writeJ(w, 400, map[string]any{"ok": false, "error": "bad multipart"})
			return
		}
		var name string
		var size int
		var ttl string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				writeJ(w, 400, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			switch p.FormName() {
			case "file":
				name = p.FileName()
				b, _ := io.ReadAll(p)
				size = len(b)
			case "ttl_min":
				b, _ := io.ReadAll(p)
				ttl = string(b)
			}
		}
		if name == "" || size == 0 {
			writeJ(w, 400, map[string]any{"ok": false, "error": "缺少文件字段（file）"})
			return
		}
		writeJ(w, 200, map[string]any{
			"ok": true, "url": "https://cn.dl.xiusoft.cn/f/20260926/abc123/" + name,
			"id": "abc123", "name": name, "size": size, "sha256": "x", "expires_at": 1, "ttl_min": ttl,
		})
	})
	mux.HandleFunc("/api/upload/init", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
			SHA256   string `json:"sha256"`
			Chunks   int    `json:"chunks"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.initHits++
		id := fmt.Sprintf("up%d", f.initHits)
		f.gotSHA[id] = req.SHA256
		f.chunksRecv[id] = map[int]string{}
		f.mu.Unlock()
		writeJ(w, 200, map[string]any{
			"ok": true, "upload_id": id, "chunk_size": f.infoChunk,
			"chunks": req.Chunks, "name": req.Filename, "size": req.Size,
		})
	})
	mux.HandleFunc("/api/upload/chunk", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UploadID string `json:"upload_id"`
			Idx      int    `json:"idx"`
			Data     string `json:"data"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		if f.failChunk > 0 {
			f.failChunk--
			f.mu.Unlock()
			writeJ(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "限流"})
			return
		}
		f.chunkHits++
		if seg, err := base64.StdEncoding.DecodeString(req.Data); err == nil {
			f.chunksRecv[req.UploadID][req.Idx] = hex.EncodeToString(seg)
		}
		f.mu.Unlock()
		writeJ(w, 200, map[string]any{"ok": true, "received": req.Idx + 1})
	})
	mux.HandleFunc("/api/upload/complete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UploadID string `json:"upload_id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		var h = sha256.New()
		for i := 0; ; i++ {
			seg, ok := f.chunksRecv[req.UploadID][i]
			if !ok {
				break
			}
			b, _ := hex.DecodeString(seg)
			h.Write(b)
		}
		got := hex.EncodeToString(h.Sum(nil))
		want := f.gotSHA[req.UploadID]
		f.mu.Unlock()
		if got != want {
			writeJ(w, 400, map[string]any{"ok": false, "error": "sha256 校验不符"})
			return
		}
		writeJ(w, 200, map[string]any{
			"ok": true, "url": "https://cn.dl.xiusoft.cn/f/20260926/big999/big.bin",
			"id": "big999", "name": "big.bin", "size": 800000, "sha256": got, "expires_at": 1,
		})
	})
	mux.HandleFunc("/api/upload/abort", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.aborts++
		f.mu.Unlock()
		writeJ(w, 200, map[string]any{"ok": true, "aborted": true})
	})
	mux.HandleFunc("/api/file/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/file/")
		f.mu.Lock()
		if f.knownIDs == nil || !f.knownIDs[id] {
			// 服务端真实行为：文件不在此站 → 200 + deleted=false（不是 404）
			f.mu.Unlock()
			writeJ(w, 200, map[string]any{"ok": true, "deleted": false})
			return
		}
		f.deleted = append(f.deleted, id)
		delete(f.knownIDs, id) // 幂等：再删一次即 deleted=false
		f.mu.Unlock()
		writeJ(w, 200, map[string]any{"ok": true, "deleted": true})
	})
	return f.serve(mux)
}

// serve 统一入口：记录鉴权头，并在 requireSig 时**按协议验签**（验证客户端真的签对了）。
//
// 验签要读请求体摘要，故读入后**原样放回**，不影响后续 handler。
func (f *fakeFiles) serve(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		f.mu.Lock()
		f.seenTokens = append(f.seenTokens, r.Header.Get(tokenHeader))
		if r.Method != http.MethodGet { // 读接口不带签名（公开），只统计写请求
			f.seenNonces = append(f.seenNonces, r.Header.Get(HeaderNonce))
			f.seenCerts = append(f.seenCerts, r.Header.Get(HeaderCert))
		}
		need, pub := f.requireSig, f.pub
		f.mu.Unlock()

		// 只有**写**操作需要签名（读接口 /api/info、/f/… 公开）
		if need && r.Method != http.MethodGet {
			if _, _, ok := verifyNodeSig(r, body, pub); !ok {
				f.mu.Lock()
				f.badSigs++
				f.mu.Unlock()
				writeJ(w, 401, map[string]any{"ok": false, "error": "节点签名校验失败"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJ(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// setupClient 起两个假站点（主/备）并返回客户端与两个 fake
func setupClient(t *testing.T, defRegion string) (*Client, *fakeFiles, *fakeFiles) {
	t.Helper()
	pub, _ := installTestIdentity(t) // 节点身份：**零配置**上传的正途
	primary, backup := newFake(), newFake()
	primary.requireSig, primary.pub = true, pub
	backup.requireSig, backup.pub = true, pub
	s1 := httptest.NewServer(primary.handler())
	s2 := httptest.NewServer(backup.handler())
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)
	primary.url, backup.url = s1.URL, s2.URL

	t.Setenv("WS_FILES_TIMEOUT", "10")
	t.Setenv("WS_FILES_TTL_MIN", "60")
	if defRegion == "hk" {
		t.Setenv("WS_FILES_BASE_HK", s1.URL)
		t.Setenv("WS_FILES_BASE_CN", s2.URL)
	} else {
		t.Setenv("WS_FILES_BASE_CN", s1.URL)
		t.Setenv("WS_FILES_BASE_HK", s2.URL)
	}
	t.Setenv("WS_FILES_REGION", "")
	return New(defRegion), primary, backup
}

// setupClientNoAuth 同 setupClient，但**既无身份也无令牌**（验证"禁用"路径）
func setupClientNoAuth(t *testing.T, defRegion string) (*Client, *fakeFiles, *fakeFiles) {
	t.Helper()
	t.Setenv(EnvKeyPath, filepath.Join(t.TempDir(), "no-such-key"))
	t.Setenv(EnvNodeID, "")
	primary, backup := newFake(), newFake()
	s1 := httptest.NewServer(primary.handler())
	s2 := httptest.NewServer(backup.handler())
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)
	primary.url, backup.url = s1.URL, s2.URL

	t.Setenv("WS_FILES_TOKEN", "")
	t.Setenv("WS_FILES_TIMEOUT", "10")
	t.Setenv("WS_FILES_TTL_MIN", "60")
	if defRegion == "hk" {
		t.Setenv("WS_FILES_BASE_HK", s1.URL)
		t.Setenv("WS_FILES_BASE_CN", s2.URL)
	} else {
		t.Setenv("WS_FILES_BASE_CN", s1.URL)
		t.Setenv("WS_FILES_BASE_HK", s2.URL)
	}
	t.Setenv("WS_FILES_REGION", "")
	return New(defRegion), primary, backup
}

func writeTemp(t *testing.T, name string, size int) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 零配置口径：**没有令牌也照常可用** —— 靠本机节点私钥签名（ws-core 自动生成的 id_ed25519）。
func TestNodeIdentityWithoutAnyConfig(t *testing.T) {
	pub, node := installTestIdentity(t)
	c := New("cn")
	if !c.Enabled() {
		t.Fatal("有节点身份时应可用（这正是『零配置』的含义）")
	}
	if c.NodeID() != node {
		t.Fatalf("NodeID 应为 %s，实际 %s", node, c.NodeID())
	}
	if c.token != "" {
		t.Fatal("测试前提：未配令牌")
	}

	// 真发一次，且让服务端验签 —— 证明"可用"不是嘴上说说
	f := newFake()
	f.requireSig, f.pub = true, pub
	srv := newHTTPTestServer(t, f)
	t.Setenv("WS_FILES_BASE_CN", srv)
	t.Setenv("WS_FILES_BASE_HK", srv)
	c = New("cn")
	if _, err := c.Upload(context.Background(), writeTemp(t, "a.txt", 100), "a.txt"); err != nil {
		t.Fatal(err)
	}
	if f.badSigs != 0 {
		t.Fatalf("签名未通过服务端验签（badSigs=%d）", f.badSigs)
	}
}

// 既读不到私钥、也没有令牌 ⇒ 禁用，且原因可操作；Upload 直接返回 ErrDisabled（不做无意义请求）
func TestNoKeyNoTokenIsDisabled(t *testing.T) {
	c, _, _ := setupClientNoAuth(t, "cn")
	if c.Enabled() {
		t.Fatal("无身份、无令牌时应为禁用")
	}
	if _, err := c.Upload(context.Background(), "/etc/hosts", "hosts"); err != ErrDisabled {
		t.Fatalf("应返回 ErrDisabled，实际 %v", err)
	}
}

// 签名模式**不发令牌头**（令牌是可选的第二条通道，不该被无谓地广播出去）
func TestSignedRequestCarriesNoTokenHeader(t *testing.T) {
	c, primary, _ := setupClient(t, "cn")
	if _, err := c.Upload(context.Background(), writeTemp(t, "a.txt", 100), "a.txt"); err != nil {
		t.Fatal(err)
	}
	primary.mu.Lock()
	defer primary.mu.Unlock()
	for _, tok := range primary.seenTokens {
		if tok != "" {
			t.Fatalf("签名模式不应发送令牌头，实际 %q", tok)
		}
	}
}

// 服务端验签不过（如本机身份与登记公钥不一致）⇒ 401 必须带可操作的排障提示
func TestUnauthorizedCarriesHint(t *testing.T) {
	_, _ = installTestIdentity(t) // 客户端自己的身份
	otherPub, _, _ := ed25519.GenerateKey(nil)
	c, primary, backup := setupClientNoAuth(t, "cn")
	// 手工启用签名通道，但服务端认的是**另一把公钥** —— 模拟"换了机器/身份"
	localPub, _ := installTestIdentity(t)
	id, err := LoadIdentity(os.Getenv(EnvKeyPath), "")
	if err != nil {
		t.Fatal(err)
	}
	_ = localPub
	c.signer, c.signErr = id, nil
	primary.mu.Lock()
	primary.requireSig, primary.pub = true, otherPub
	backup.mu.Lock()
	backup.requireSig, backup.pub = true, otherPub
	backup.mu.Unlock()
	primary.mu.Unlock()

	_, err = c.Upload(context.Background(), writeTemp(t, "a.txt", 100), "a.txt")
	if err == nil {
		t.Fatal("签名不被认可时应报错")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "id_ed25519") {
		t.Fatalf("401 应带排障提示（指明私钥/名册/证书方向），实际: %v", err)
	}
}

// 小文件走单发，文件名/ttl 正确送达
func TestUploadSingleShot(t *testing.T) {
	c, primary, _ := setupClient(t, "cn")
	if !c.Enabled() {
		t.Fatal("应启用")
	}
	p := writeTemp(t, "小 图.png", 1000)
	res, err := c.Upload(context.Background(), p, "小 图.png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.URL, "/f/") || res.ID == "" {
		t.Fatalf("URL/ID 异常: %+v", res)
	}
	if res.Chunked {
		t.Fatal("小文件不应走分块")
	}
	if res.Base != primary.url {
		t.Fatalf("Base 应为实际站点 %s，实际 %s", primary.url, res.Base)
	}
	if primary.singleHits != 1 {
		t.Fatalf("单发应恰好 1 次，实际 %d", primary.singleHits)
	}
}

// 大文件自动走分块，且服务端按整份 sha256 校验通过
func TestUploadChunked(t *testing.T) {
	c, primary, _ := setupClient(t, "cn")
	size := SingleShotRawMax + 1000 // 800048 B
	p := writeTemp(t, "big.bin", size)
	res, err := c.Upload(context.Background(), p, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Chunked {
		t.Fatal("大文件应走分块")
	}
	want := 2 // ceil(800048 / 400000)
	if primary.chunkHits != want {
		t.Fatalf("分块数应为 %d，实际 %d", want, primary.chunkHits)
	}
	if primary.singleHits != 0 {
		t.Fatal("大文件不应走单发")
	}
	if res.SHA256 == "" {
		t.Fatal("complete 应回 sha256")
	}
}

// region=hk ⇒ 优先 hk 站点；region=cn ⇒ 优先 cn 站点
func TestRegionPreference(t *testing.T) {
	c, s1, s2 := setupClient(t, "hk")
	if _, err := c.Upload(context.Background(), writeTemp(t, "a.txt", 10), "a.txt"); err != nil {
		t.Fatal(err)
	}
	if s1.singleHits != 1 || s2.singleHits != 0 {
		t.Fatalf("hk 优先应命中 hk：hk=%d cn=%d", s1.singleHits, s2.singleHits)
	}

	c2, s1b, s2b := setupClient(t, "cn")
	if _, err := c2.Upload(context.Background(), writeTemp(t, "b.txt", 10), "b.txt"); err != nil {
		t.Fatal(err)
	}
	if s1b.singleHits != 1 || s2b.singleHits != 0 {
		t.Fatalf("cn 优先应命中 cn：cn=%d hk=%d", s1b.singleHits, s2b.singleHits)
	}
}

// WS_FILES_REGION 显式设置时覆盖 defaultRegion
func TestRegionEnvOverride(t *testing.T) {
	c, _, hkStation := setupClient(t, "cn") // defRegion=cn ⇒ s1=cn、s2=hk
	t.Setenv("WS_FILES_REGION", "hk")
	c = New("cn")
	if got := c.PrimaryBase(); got != hkStation.url {
		t.Fatalf("WS_FILES_REGION=hk 应优先 hk 站点，实际 %s（期望 %s）", got, hkStation.url)
	}
}

// 首选站点 500 ⇒ 自动转移到备站，结果仍成功
func TestFailoverToBackup(t *testing.T) {
	c, primary, backup := setupClient(t, "cn")
	primary.failSingle = 1
	res, err := c.Upload(context.Background(), writeTemp(t, "f.bin", 100), "f.bin")
	if err != nil {
		t.Fatal(err)
	}
	if backup.singleHits != 1 {
		t.Fatalf("应转移到备站，备站命中 %d", backup.singleHits)
	}
	if res.Base != c.bases()[1] {
		t.Fatalf("Base 应为备站，实际 %s", res.Base)
	}
}

// 429 限流 ⇒ 退避后重试成功（不转移站点、不损坏内容）
func TestRetryOn429(t *testing.T) {
	c, primary, backup := setupClient(t, "cn")
	primary.failChunk = 1 // 第一块限流
	p := writeTemp(t, "big.bin", SingleShotRawMax+100)
	res, err := c.Upload(context.Background(), p, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if res.Base == "" || backup.chunkHits != 0 {
		t.Fatal("限流应在同站重试成功，不应转移站点")
	}
	if primary.chunkHits != 2 {
		t.Fatalf("重试后应成功上传 2 块，实际 %d", primary.chunkHits)
	}
}

// 两站都失败 ⇒ 报错信息含两个站点
func TestBothBasesFail(t *testing.T) {
	c, primary, backup := setupClient(t, "cn")
	primary.failSingle, backup.failSingle = 1, 1
	_, err := c.Upload(context.Background(), writeTemp(t, "x.bin", 10), "x.bin")
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "已尝试 2 个站点") {
		t.Fatalf("错误应说明尝试了两个站点: %v", err)
	}
}

// 目录/空文件/超限一律拒绝
func TestUploadRejects(t *testing.T) {
	c, _, _ := setupClient(t, "cn")
	ctx := context.Background()
	if _, err := c.Upload(ctx, t.TempDir(), "d"); err == nil {
		t.Fatal("目录应被拒")
	}
	empty := filepath.Join(t.TempDir(), "e.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(ctx, empty, "e.txt"); err == nil {
		t.Fatal("空文件应被拒")
	}
	if _, err := c.Upload(ctx, filepath.Join(t.TempDir(), "nope"), "n"); err == nil {
		t.Fatal("不存在的文件应被拒")
	}
}

// 回收：DELETE /api/file/{id}
func TestDelete(t *testing.T) {
	c, primary, _ := setupClient(t, "cn")
	primary.knownIDs["abc123"] = true
	if err := c.Delete(context.Background(), "abc123"); err != nil {
		t.Fatal(err)
	}
	if len(primary.deleted) != 1 || primary.deleted[0] != "abc123" {
		t.Fatalf("回收请求异常: %v", primary.deleted)
	}
}

// 已投递在备站时，Delete（不知道站点）必须问到备站才算成功，不能因首站「200 + deleted=false」误判
func TestDeleteFindsFileOnBackupSite(t *testing.T) {
	c, primary, backup := setupClient(t, "cn")
	backup.knownIDs["on-backup"] = true
	if err := c.Delete(context.Background(), "on-backup"); err != nil {
		t.Fatalf("应能在备站删到文件: %v", err)
	}
	if len(backup.deleted) != 1 {
		t.Fatalf("备站应被删到: %v", backup.deleted)
	}
	if len(primary.deleted) != 0 {
		t.Fatalf("首站不应误报删除: %v", primary.deleted)
	}

	// 两站都没有 → 返回错误（而不是静默成功）
	if err := c.Delete(context.Background(), "nowhere"); err == nil {
		t.Fatal("两站都没有该文件时应报错")
	}
}

// DeleteAt 只问指定站点，避免空跑
func TestDeleteAt(t *testing.T) {
	c, primary, backup := setupClient(t, "cn")
	backup.knownIDs["bb"] = true
	ctx := context.Background()

	if err := c.DeleteAt(ctx, backup.url, "bb"); err != nil {
		t.Fatalf("指定备站删除应成功: %v", err)
	}
	if len(primary.deleted) != 0 {
		t.Fatal("DeleteAt 不应请求其他站点")
	}
	if err := c.DeleteAt(ctx, backup.url, "nope"); err == nil {
		t.Fatal("该站没有该文件应报错")
	}
	if err := c.DeleteAt(ctx, "", "bb"); err == nil {
		// 空站点 ⇒ 退化为 Delete（此时 backup 已被删，两站都没有）
		t.Fatal("空站点退化后仍应报错")
	}
}

// 文件名清洗：去目录、去控制字符、限长
func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"a/b/c.png":        "c.png",
		"ok-name_1.txt":    "ok-name_1.txt",
		"含 中文 名.png":       "含 中文 名.png",
		"  .hidden  ":      "hidden",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := sanitizeName("x"); got != "x" {
		t.Errorf("普通名不应被改动: %q", got)
	}
	if len([]rune(sanitizeName(strings.Repeat("长", 300)))) > 100 {
		t.Error("长名应被截断到 100 字符")
	}
}

func (f *fakeFiles) counts() (single, chunk, init int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.singleHits, f.chunkHits, f.initHits
}
