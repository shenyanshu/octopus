import type { RelayLogOverview } from "@/api/log";

// 三态计价显示: running/null 未到位给 '-', terminal unknown 给"未计价",
// group_reference 给 ≈前缀与参考模型名, 其余给精确值。
// 抽出来纯函数便于 node:test 覆盖三态, 不触碰 UI 组件。
export interface CostDisplay {
  value: string;
  // 仅 group_reference 且带参考名时带该提示。
  tooltip?: string;
  valueClassName: string;
}

export function formatLogCost(
  log: Pick<
    RelayLogOverview,
    "cost" | "cost_known" | "cost_source" | "cost_reference_model"
  >,
  t: (key: string, values?: Record<string, string>) => string,
): CostDisplay {
  if (log.cost === null || !log.cost_known) {
    return {
      value:
        log.cost_source === "unknown" ? t("costUnknown") : t("costPending"),
      valueClassName: "font-medium text-emerald-600 dark:text-emerald-400",
    };
  }
  if (log.cost_source === "group_reference") {
    return {
      value: `≈${log.cost.toFixed(6)}`,
      tooltip: log.cost_reference_model
        ? t("estimatedByGroup", { model: log.cost_reference_model })
        : undefined,
      valueClassName:
        "font-medium text-emerald-600 dark:text-emerald-400 underline decoration-dotted decoration-emerald-500/60 underline-offset-2",
    };
  }
  return {
    value: log.cost.toFixed(6),
    valueClassName: "font-medium text-emerald-600 dark:text-emerald-400",
  };
}
