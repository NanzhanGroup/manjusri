// Package token_cache — 词元缓存共享配置类型
package token_cache

// Config 词元缓存配置
type Config struct {
	Enabled      bool                         `yaml:"enabled"`
	AgentID      string                       `yaml:"agent_id"`
	AgentName    string                       `yaml:"agent_name"`
	Server       ServerConfig                 `yaml:"server"`
	Storage      StorageConfig                `yaml:"storage"`
	Redis        RedisConfig                  `yaml:"redis"`
	Local        LocalConfig                  `yaml:"local"`
	CacheTTL     int                          `yaml:"cache_ttl"`
	Bloom        BloomConfig                  `yaml:"bloom"`
	Circuit      CircuitConfig                `yaml:"circuit_breaker"`
	Stats        bool                         `yaml:"stats"`
	LLMProviders map[string]LLMProviderConfig `yaml:"llm_providers"`
	PurposeMap   map[string]string            `yaml:"purpose_map"`
	Upstream     UpstreamConfig               `yaml:"upstream"`
	Embedding    EmbeddingConfig              `yaml:"embedding"`
	Log          LogConfig                    `yaml:"log"`
	FileCache    FileCacheConfig              `yaml:"file_cache"`
}

// FileCacheConfig 文件缓存配置
type FileCacheConfig struct {
	Enabled         bool  `yaml:"enabled"`
	MaxFiles        int   `yaml:"max_files"`
	MaxFileSize     int64 `yaml:"max_file_size"`
	ContentCacheTTL int   `yaml:"content_cache_ttl"`
	DepsCacheTTL    int   `yaml:"deps_cache_ttl"`
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	UnixSocket string `yaml:"unix_socket"`
	Token      string `yaml:"token"`
	ReadOnly   bool   `yaml:"readonly"`
}

// EmbeddingConfig Embedding 引擎配置
type EmbeddingConfig struct {
	Enabled       bool              `yaml:"enabled"`
	ModelPath     string            `yaml:"model_path"`
	ClientEnabled bool              `yaml:"client_enabled"`
	Cluster       ClusteringConfig  `yaml:"cluster"`
	ApproxMatch   ApproxMatchConfig `yaml:"approx_match"`
}

// ClusteringConfig 聚类配置
type ClusteringConfig struct {
	Enabled  bool    `yaml:"enabled"`
	Branch   float64 `yaml:"branch"`
	MaxLeaf  int     `yaml:"max_leaf"`
}

// ApproxMatchConfig 近似匹配配置
type ApproxMatchConfig struct {
	Enabled   bool    `yaml:"enabled"`
	Threshold float64 `yaml:"threshold"`
}

// LogConfig 缓存请求日志配置
type LogConfig struct {
	Dir              string `yaml:"dir"`
	BatchSize        int    `yaml:"batch_size"`
	FlushIntervalSec int    `yaml:"flush_interval_sec"`
	RetentionDays    int    `yaml:"retention_days"`
	Enabled          bool   `yaml:"enabled"`
}

// LLMProviderConfig 大模型供应商配置
type LLMProviderConfig struct {
	BaseURL         string `yaml:"base_url"`
	APIKey          string `yaml:"api_key,omitempty"`
	APIKeyEncrypted string `yaml:"api_key_encrypted,omitempty"`
	Model           string `yaml:"model"`
}

// StorageConfig 持久层配置
type StorageConfig struct {
	Driver   string         `yaml:"driver"`
	SQLite   SQLiteConfig   `yaml:"sqlite"`
	Postgres PostgresConfig `yaml:"postgres"`
	Flush    FlushConfig    `yaml:"flush"`
}

// SQLiteConfig SQLite 驱动配置
type SQLiteConfig struct {
	Path string `yaml:"path"`
}

// PostgresConfig PostgreSQL 驱动配置
type PostgresConfig struct {
	DSN      string `yaml:"dsn"`
	MaxConns int    `yaml:"max_conns"`
}

// FlushConfig 写后刷配置
type FlushConfig struct {
	BatchSize int    `yaml:"batch_size"`
	Interval  string `yaml:"interval"`
	ExitFlush bool   `yaml:"exit_flush"`
}

// RedisConfig Redis 连接配置
type RedisConfig struct {
	Addr         string `yaml:"addr"`
	Password     string `yaml:"password"`
	DB           int    `yaml:"db"`
	PoolSize     int    `yaml:"pool_size"`
	MinIdle      int    `yaml:"min_idle"`
	MaxRetries   int    `yaml:"max_retries"`
	DialTimeout  string `yaml:"dial_timeout"`
	ReadTimeout  string `yaml:"read_timeout"`
	WriteTimeout string `yaml:"write_timeout"`
}

// LocalConfig L1 本地缓存配置
type LocalConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Shards   int    `yaml:"shards"`
	MaxItems int    `yaml:"max_items"`
	TTL      string `yaml:"ttl"`
}

// BloomConfig 布隆过滤器配置
type BloomConfig struct {
	Enabled           bool  `yaml:"enabled"`
	ExpectedItems     int64 `yaml:"expected_items"`
	FalsePositiveRate float64 `yaml:"false_positive_rate"`
}

// CircuitConfig 熔断器配置
type CircuitConfig struct {
	Enabled          bool   `yaml:"enabled"`
	FailureThreshold int    `yaml:"failure_threshold"`
	RecoveryTime     string `yaml:"recovery_time"`
}

// UpstreamAuth 上游认证配置
type UpstreamAuth struct {
	Type  string `yaml:"type"`
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// UpstreamConfig 上游词元缓存配置
type UpstreamConfig struct {
	URL   string       `yaml:"url"`
	Token string       `yaml:"token"`
	Auth  UpstreamAuth `yaml:"auth"`
}
