package op

import "errors"

// ErrRevisionConflict 是乐观锁冲突的稳定哨兵错误:
// 更新提交的 expected revision 与当前库内不匹配(渠道被并发编辑、删除后重建或导入覆盖),
// handler 据此返回 409。渠道不存在时同样返回此错误: 删除后重建使旧令牌失效, 语义一致。
var ErrRevisionConflict = errors.New("channel revision conflict")

// ErrRevisionRequired 表示全量更新未提交 expected revision 令牌。
// handler 据此返回 400: 空串或缺失的 revision 不允许绕过乐观锁。
var ErrRevisionRequired = errors.New("expected revision is required")
