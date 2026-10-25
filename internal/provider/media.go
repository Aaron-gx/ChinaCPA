// media.go 图片 / 视频生成能力接口：Provider 可选实现，网关据此暴露
// /v1/images/generations 与视频任务 API（火山方舟风格）。协议设计参考
// 火山引擎 Ark（/api/v3/images/generations、/api/v3/contents/generations/tasks）
// 与 OpenAI Images 的通用形态。
package provider

import (
	"context"
	"time"

	"chinacpa/internal/model"
)

// ImageRequest 一次图片生成请求（OpenAI Images / Ark Images 归一化）。
type ImageRequest struct {
	Prompt  string
	Ratio   string // "1:1" / "16:9" ...（Seedream 网页通道用比例而非分辨率）
	Size    string // "2048x2048" 等，未给 Ratio 时据此推导比例
	N       int    // 期望张数（上游不保证满足）
	Timeout time.Duration
}

// ImageResult 图片生成结果。
type ImageResult struct {
	URLs           []string
	RevisedPrompt  string
	ConversationID string
}

// ImageGenerator 支持图片生成的 Provider。
type ImageGenerator interface {
	// GenerateImage 阻塞直到出图或失败；实现负责自动确认轮与失败关键词判定。
	GenerateImage(ctx context.Context, a *model.Account, modelName string, req ImageRequest) (*ImageResult, error)
}

// VideoRequest 一次视频生成请求（Ark content 命令行参数已归一化）。
type VideoRequest struct {
	Prompt     string
	Ratio      string        // 默认 16:9
	Duration   int           // 秒，4~30（模型不支持时由 provider 收敛）
	Resolution string        // 720p / 1080p（上游网页通道忽略）
	Timeout    time.Duration // 轮询总预算（0 = 默认）
}

// VideoTaskHandle 上游任务句柄（provider 私有，网关不透明保存）。
type VideoTaskHandle struct {
	ConversationID string
	Model          string
	Prompt         string
	Extra          map[string]string
}

// VideoTaskStatus 任务状态快照。Status: queued / running / succeeded / failed。
type VideoTaskStatus struct {
	Status        string
	VideoURL      string
	CoverURL      string
	VideoVID      string
	Error         string
	Unwatermarked bool
}

// VideoGenerator 支持视频生成（异步任务）的 Provider。
type VideoGenerator interface {
	// CreateVideoTask 提交生成（读到 SSE_ACK 即返回句柄）。
	CreateVideoTask(ctx context.Context, a *model.Account, modelName string, req VideoRequest) (*VideoTaskHandle, error)
	// PollVideoTask 查询一次进度（内部处理确认轮）；不阻塞等待完成。
	PollVideoTask(ctx context.Context, a *model.Account, h *VideoTaskHandle) (*VideoTaskStatus, error)
}
