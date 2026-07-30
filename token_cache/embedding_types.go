// Package token_cache — 词元缓存共享类型
// 本包是 token-cache 服务的共享接口定义，供 chat 等模块使用
package token_cache

import "math"

// EmbeddingEngine 文本向量化引擎接口
type EmbeddingEngine interface {
	Encode(text string) ([]float32, error)
	EncodeBatch(texts []string) ([][]float32, error)
	Dim() int
	Close()
}

// CosineSimilarity 计算两个向量的余弦相似度
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := 0; i < len(a); i++ {
		dot += float64(a[i] * b[i])
		normA += float64(a[i] * a[i])
		normB += float64(b[i] * b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
