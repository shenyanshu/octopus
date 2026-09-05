package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/gin-gonic/gin"
)

// grantPreviewRequest 仅收一个 pattern 字段: 不复用其它渠道结构, 免得耦合出额外校验。
type grantPreviewRequest struct {
	Pattern string `json:"pattern"`
}

// previewChannelGrants 按正则预览授权候选, 供分组页在批量加入前先看会选中哪些。
// 与 listChannelGrant 同源取数, 只多一层正则过滤: 默认大小写敏感, (?i) 由调用方显式声明。
// 规则的校验与编译复用 model.CompileGroupPattern, 与分组自动补充同一口径, 不各自另写一套。
// 空模式不返回全集而返回空集: 避免误把"没填"当成"全选", 批量加入前必须显式给定条件。
// 不可用候选照常返回, 由前端在批量加入时跳过: 预览的职责是"看会命中什么", 不是"看会加成什么"。
func previewChannelGrants(c *gin.Context) {
	var req grantPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}

	// 单一编译入口: 空串返回 nil 正则表示关闭规则, 非法/过长返回稳定错误文案。
	// 历史错误契约不变: "pattern exceeds maximum length" 与 "invalid regex pattern" 仍由此处映射。
	re, err := model.CompileGroupPattern(req.Pattern)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if re == nil {
		resp.Success(c, make([]model.ChannelGrantCandidate, 0))
		return
	}

	candidates := op.ChannelGrantCandidates()
	matched := make([]model.ChannelGrantCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if re.MatchString(candidate.ModelName) {
			matched = append(matched, candidate)
		}
	}
	// 始终返回非 nil 切片: 前端按数组处理, 不接受 null。
	resp.Success(c, matched)
}
