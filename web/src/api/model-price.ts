// 价格输入校验与派生状态(无后端依赖), 供表单与测试复用。
// 不引依赖, 纯函数; 失败一律 null/空对象, 绝不静默把非法输入转成 0(免费是显式决策)。

export interface LLMPrice {
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
}

export type PriceField = keyof LLMPrice;

const PRICE_FIELDS: readonly PriceField[] = [
  "input",
  "output",
  "cache_read",
  "cache_write",
];

/** 非空且为有限非负数, 否则 null。空/空白/NaN/Infinity/负数都拒。 */
export function parsePriceInput(value: string): number | null {
  const trimmed = value.trim();
  if (trimmed === "") return null;
  const parsed = Number(trimmed);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : null;
}

/** 四项解析, 任一项空/非法整体返回 null, 用于 create/update 拒绝提交。 */
export function parseEditPrices(
  values: Record<PriceField, string>,
): Record<PriceField, number> | null {
  const result = {} as Record<PriceField, number>;
  for (const field of PRICE_FIELDS) {
    const parsed = parsePriceInput(values[field]);
    if (parsed === null) return null;
    result[field] = parsed;
  }
  return result;
}

/**
 * 编辑浮层初始值: 已知回显价格, 未知留空引导填写四价。
 * 已知 = price_known 为真且 price 非空(契约); 否则全部空。
 */
export function initialEditValues(
  priceKnown: boolean,
  price: Record<PriceField, number> | null,
): Record<PriceField, string> {
  if (!priceKnown || !price) {
    return { input: "", output: "", cache_read: "", cache_write: "" };
  }
  return {
    input: price.input.toString(),
    output: price.output.toString(),
    cache_read: price.cache_read.toString(),
    cache_write: price.cache_write.toString(),
  };
}

/** 仅手动价可放弃回自动: 恢复自动定价按钮的显隐条件。 */
export function shouldShowRestoreAuto(source: "manual" | "auto"): boolean {
  return source === "manual";
}

/**
 * 编辑互斥: pending 时入口拒, 不依赖按钮 disabled。两个动作共享同一 pending 概念:
 * 保存(update)在途则不能再触发恢复, 反之亦然; 防止交错写。
 */
export function canInteract(
  updatePending: boolean,
  restorePending: boolean,
): boolean {
  return !updatePending && !restorePending;
}
