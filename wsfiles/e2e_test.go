package wsfiles

// e2e_test.go —— 对**真实 ws-files 服务**的端到端测试（默认跳过）。
//
// 启用方式（不要用生产令牌做破坏性测试，建议本地起一个实例）：
//
//	# 节点签名（正途：客户端零配置，服务端以 --roster 或 --trust-root 启动）
//	WSFILES_E2E_BASE=http://127.0.0.1:9547 \
//	  go test ./wsfiles/ -run TestE2E -v -count=1
//	# 令牌（服务端以 --token 启动时的调试通道）
//	WSFILES_E2E_BASE=http://127.0.0.1:9547 WSFILES_E2E_TOKEN=xxx \
//	  go test ./wsfiles/ -run TestE2E -v -count=1
//
// 覆盖：小文件单发 / 大文件分块 / 下载回读 sha256 一致 / 回收后 404。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func e2eClient(t *testing.T) *Client {
	t.Helper()
	base := os.Getenv("WSFILES_E2E_BASE")
	tok := os.Getenv("WSFILES_E2E_TOKEN")
	if base == "" {
		t.Skip("未设置 WSFILES_E2E_BASE，跳过真实服务端到端测试")
	}
	t.Setenv("WS_FILES_TOKEN", tok)
	if tok == "" && os.Getenv("WSFILES_E2E_KEY") == "" {
		// 正途：本机节点身份（data/family/id_ed25519）自动签名，无需任何配置
		t.Log("未设 WSFILES_E2E_KEY / WSFILES_E2E_TOKEN ⇒ 用本机节点身份签名（零配置口径）")
	}
	if k := os.Getenv("WSFILES_E2E_KEY"); k != "" {
		t.Setenv(EnvKeyPath, k)
	}
	t.Setenv("WS_FILES_BASE_CN", base)
	t.Setenv("WS_FILES_BASE_HK", base)
	t.Setenv("WS_FILES_REGION", "cn")
	t.Setenv("WS_FILES_TTL_MIN", "10")
	return New("cn")
}

func e2eFile(t *testing.T, name string, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte((i*7 + 13) % 251)
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func shaOfFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func download(t *testing.T, url string) ([]byte, int) {
	t.Helper()
	hc := &http.Client{Timeout: 60 * time.Second}
	resp, err := hc.Get(url)
	if err != nil {
		t.Fatalf("下载失败 %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode
}

func TestE2EUploadAndDownload(t *testing.T) {
	c := e2eClient(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		size    int
		chunked bool
	}{
		{"small-小图.png", 1234, false},
		{"big-大文件.bin", SingleShotRawMax*2 + 77, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := e2eFile(t, tc.name, tc.size)
			res, err := c.Upload(ctx, local, tc.name)
			if err != nil {
				t.Fatalf("投递失败: %v", err)
			}
			if res.Chunked != tc.chunked {
				t.Errorf("分块判定不符：got %v want %v（size=%d）", res.Chunked, tc.chunked, tc.size)
			}
			wantSHA := shaOfFile(t, local)
			if res.SHA256 != "" && res.SHA256 != wantSHA {
				t.Errorf("服务端 sha 不符：got %s want %s", res.SHA256, wantSHA)
			}
			body, code := download(t, res.URL)
			if code != 200 {
				t.Fatalf("下载 HTTP %d，URL=%s", code, res.URL)
			}
			if len(body) != tc.size {
				t.Errorf("下载大小不符：got %d want %d", len(body), tc.size)
			}
			h := sha256.Sum256(body)
			if hex.EncodeToString(h[:]) != wantSHA {
				t.Error("下载内容 sha256 与本地不一致（内容被改写）")
			}
			t.Logf("✅ %s 投递成功 base=%s chunked=%v url=%s", tc.name, res.Base, res.Chunked, res.URL)

			// 回收后应 404（验证 Delete 生效）
			if err := c.Delete(ctx, res.ID); err != nil {
				t.Errorf("回收失败: %v", err)
			}
			if _, code := download(t, res.URL); code == 200 {
				t.Error("回收后仍可下载（Delete 未生效）")
			}
			_ = fmt.Sprint()
		})
	}
}
