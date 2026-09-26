package wsfiles

import (
	"os"
	"path/filepath"
	"testing"
)

// 非密配置项：进程环境 > /etc/environment > $WS_PATH/.env
func TestConfigValueFallbackToWSEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "# 注释\nWS_FILES_TESTKEY=from-dotenv\nQUOTED=\"quoted-value\"\nSPACED = nope\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WS_PATH", dir)
	t.Setenv("WS_FILES_TESTKEY", "")

	if got := configValue("WS_FILES_TESTKEY"); got != "from-dotenv" {
		t.Errorf("应从 $WS_PATH/.env 读取，实际 %q", got)
	}
	if got := configValue("QUOTED"); got != "quoted-value" {
		t.Errorf("引号应被去掉，实际 %q", got)
	}
	// 进程环境优先于文件
	t.Setenv("WS_FILES_TESTKEY", "from-env")
	if got := configValue("WS_FILES_TESTKEY"); got != "from-env" {
		t.Errorf("进程环境应优先，实际 %q", got)
	}
	if got := configValue("WS_FILES_NOT_EXIST_KEY"); got != "" {
		t.Errorf("不存在的键应返回空，实际 %q", got)
	}
}

// SECURITY/P0 不变式：令牌**只从进程环境读**，不得从 /etc/environment 或 $WS_PATH/.env 取。
func TestTokenNeverReadFromFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("WS_FILES_TOKEN=leaked-from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WS_PATH", dir)
	t.Setenv("WS_FILES_TOKEN", "")
	t.Setenv("WS_FILES_REGION", "cn")

	c := New("cn")
	if c.token != "" {
		t.Fatal("令牌不得从 $WS_PATH/.env 读取（SECURITY/P0）")
	}
	if !c.Anonymous() {
		t.Fatal("无令牌时应走匿名模式（而非被 .env 里的假令牌启用）")
	}
	t.Setenv("WS_FILES_TOKEN", "from-env")
	if c := New("cn"); !c.Enabled() || c.Anonymous() {
		t.Fatal("进程环境提供令牌时应启用，且不再是匿名模式")
	}
}

// 非密的策略项可以从 $WS_PATH/.env 生效（方便部署时集中配置）
func TestDeliveryModeAndRegionFromDotenv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"),
		[]byte("WS_FILE_DELIVERY=link\nWS_FILES_REGION=hk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WS_PATH", dir)
	t.Setenv("WS_FILE_DELIVERY", "")
	t.Setenv("WS_FILES_REGION", "")

	if got := DeliveryMode(); got != ModeLink {
		t.Errorf("WS_FILE_DELIVERY 应能从 .env 生效，实际 %q", got)
	}
	// 默认 cn 站点，但 region=hk 覆盖
	t.Setenv("WS_FILES_BASE_CN", "https://cn.example")
	t.Setenv("WS_FILES_BASE_HK", "https://hk.example")
	t.Setenv("WS_FILES_TOKEN", "x")
	if got := New("cn").PrimaryBase(); got != "https://hk.example" {
		t.Errorf("region=hk 应优先 hk，实际 %q", got)
	}
}
