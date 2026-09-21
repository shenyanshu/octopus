package relay

import (
	"context"
	"testing"
)

// TestNewRequestStateCostSourceInitialized 验证新建请求的 cost_source 字段在首次事件(running)
// 和 committed 状态下都不是空串, 而是 "unknown" — 枚举契约要求非空。
func TestNewRequestStateCostSourceInitialized(t *testing.T) {
	req := newRequestState(context.Background(), "test-model", 0, 0, "", 0)

	if req.CostSource != "unknown" {
		t.Fatalf("新建请求 cost_source 应为 unknown, got %q", req.CostSource)
	}
	if req.CostKnown {
		t.Fatal("新建请求 cost_known 应为 false")
	}
	if req.Cost != nil {
		t.Fatalf("新建请求 cost 应为 nil, got %v", *req.Cost)
	}
	if req.CostReferenceModel != nil {
		t.Fatalf("新建请求 cost_reference_model 应为 nil, got %v", *req.CostReferenceModel)
	}

	// 模拟 committed 状态: markCommitted 不修改 cost 字段, cost_source 应保持 unknown。
	req.markCommitted(false)
	if req.CostSource != "unknown" {
		t.Fatalf("committed 状态 cost_source 应保持 unknown, got %q", req.CostSource)
	}
}
