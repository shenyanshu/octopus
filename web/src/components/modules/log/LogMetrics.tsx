import type { CSSProperties } from "react";
import {
  ArrowDownToLine,
  ArrowUpFromLine,
  Clock,
  Cpu,
  Database,
  DollarSign,
} from "lucide-react";
import { useTranslations } from "use-intl";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import type { RelayLogOverview } from "@/api/log";
import { cn } from "@/lib/utils";
// cost 三态派生抽为纯函数(formatLogCost), 便于 node:test 覆盖 running/unknown/group_reference.
// formatTime 将后端 RFC3339 时间转换为本地时分秒。
import { formatLogCost } from "./log-cost";

function formatTime(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() === 1) return "--";
  return date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

// formatMilliseconds 将毫秒转换为紧凑耗时文本。
function formatMilliseconds(value: number) {
  const milliseconds = Math.max(0, value);
  if (milliseconds < 1000) return `${Math.round(milliseconds)}ms`;
  return `${(milliseconds / 1000).toFixed(2)}s`;
}

// LogMetrics 渲染耗时, 费用和 Token 指标; card 变体用于卡片栅格, footer 变体用于弹窗底部。
export function LogMetrics({
  log,
  now,
  brandColor,
  variant,
}: {
  log: RelayLogOverview;
  now: number;
  brandColor: string;
  variant: "card" | "footer";
}) {
  const t = useTranslations("log");
  const cachedTokens = log.usage.prompt_tokens_details?.cached_tokens ?? 0;
  // 进行中的请求按共享时钟推算耗时, 结束后改用后端记录的最终耗时。
  const duration =
    log.status === "running" || log.status === "committed"
      ? formatMilliseconds(now - new Date(log.started_at).getTime())
      : formatMilliseconds(log.duration / 1_000_000);
  const metrics = [
    {
      key: "time",
      Icon: Clock,
      iconClassName: "size-3.5 shrink-0",
      iconStyle: { color: brandColor } as CSSProperties,
      value: formatTime(log.started_at),
      valueClassName: "tabular-nums",
      cellClassName: "col-span-4 whitespace-nowrap md:col-span-1",
    },
    {
      key: "duration",
      Icon: Cpu,
      iconClassName: "size-3.5 shrink-0 text-blue-500",
      value: duration,
      cellClassName: "col-span-4 md:col-span-1",
    },
    {
      key: "cost",
      Icon: DollarSign,
      iconClassName: "size-3.5 shrink-0 text-emerald-500",
      ...formatLogCost(log, t),
      cellClassName: "col-span-4 md:col-span-1",
    },
    {
      key: "prompt",
      Icon: ArrowDownToLine,
      iconClassName: "size-3.5 shrink-0 text-green-500",
      value: (log.usage.prompt_tokens - cachedTokens).toLocaleString(),
      cellClassName: "col-span-3 md:col-span-1",
    },
    {
      key: "cached",
      Icon: Database,
      iconClassName: "size-3.5 shrink-0 text-cyan-500",
      value: cachedTokens.toLocaleString(),
      cellClassName: "col-span-3 md:col-span-1",
    },
    {
      key: "completion",
      Icon: ArrowUpFromLine,
      iconClassName: "size-3.5 shrink-0 text-purple-500",
      value: log.usage.completion_tokens.toLocaleString(),
      cellClassName: "col-span-3 md:col-span-1",
    },
    {
      key: "cacheWrite",
      Icon: Database,
      iconClassName: "size-3.5 shrink-0 text-orange-500",
      value: (
        log.usage.prompt_tokens_details?.write_cached_tokens ?? 0
      ).toLocaleString(),
      cellClassName: "col-span-3 md:col-span-1",
    },
  ];

  return metrics.map((metric) => (
    <div
      key={metric.key}
      className={cn(
        "flex items-center gap-1.5",
        variant === "card" && metric.cellClassName,
      )}
    >
      <metric.Icon className={metric.iconClassName} style={metric.iconStyle} />
      {"tooltip" in metric ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span className={metric.valueClassName}>{metric.value}</span>
          </TooltipTrigger>
          <TooltipContent side="top" sideOffset={8}>
            {(metric as { tooltip: string }).tooltip}
          </TooltipContent>
        </Tooltip>
      ) : (
        <span className={metric.valueClassName}>{metric.value}</span>
      )}
    </div>
  ));
}
