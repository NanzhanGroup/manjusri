// 自动清理：后台定时删除过期记忆
package memorysvc

import (
	"fmt"
	"os"
	"time"
)

// ── 自动清理 ──

// startAutoCleanup 启动后台定时清理 goroutine
func (s *Server) startAutoCleanup() {
	maxAge := defaultCleanupMaxAge
	interval := defaultCleanupInterval

	if env := os.Getenv("MEMORY_CLEANUP_MAX_AGE"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			maxAge = d
		}
	}
	if env := os.Getenv("MEMORY_CLEANUP_INTERVAL"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			interval = d
		}
	}

	if maxAge <= 0 || interval <= 0 {
		fmt.Printf("[memory-service] 自动清理未启用（maxAge=%v, interval=%v）\n", maxAge, interval)
		return
	}

	fmt.Printf("[memory-service] 自动清理已启动: 保留 %v, 每 %v 检查一次\n", maxAge, interval)

	go func() {
		time.Sleep(1 * time.Minute)
		s.autoCleanup(maxAge)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			s.autoCleanup(maxAge)
		}
	}()
}

// autoCleanup 执行一次清理：删除超过 maxAge 的记忆
func (s *Server) autoCleanup(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge).Format("2006-01-02 15:04:05")

	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM memories WHERE created_at < ?", cutoff)
	if err != nil {
		fmt.Printf("[memory-service] 自动清理失败: %v\n", err)
		return
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		fmt.Printf("[memory-service] 自动清理: 删除了 %d 条过期记忆（截止 %s）\n", n, cutoff)
	}
}
