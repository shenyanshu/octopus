import type { CSSProperties } from "react";
import {
  ArrowDownToLine,
  ArrowUpFromLine,
  Clock,
  Cpu,
  Database,
  DollarSign,
  Gauge,
  KeyRound,
  Timer,
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
  // 缓存率取输入缓存占全部输入 Token 的比例, 无输入时为零。
  const cacheRate =
    log.usage.prompt_tokens > 0
      ? Math.round((cachedTokens / log.usage.prompt_tokens) * 100)
      : 0;
  // 请求进行中显示实时总耗时; 结束后只显示实际响应阶段耗时, 提交前结束时回退到总耗时。
  const requestActive = log.status === "running" || log.status === "committed";
  const elapsedMs = requestActive
    ? now - new Date(log.started_at).getTime()
    : log.duration / 1_000_000;
  const responseMs = (log.stream_duration || log.response_duration) / 1_000_000;
  const duration = formatMilliseconds(
    !requestActive && responseMs > 0 ? responseMs : elapsedMs,
  );
  // 首字时间仅流式响应存在, 非流式或尚未提交时留空。
  const firstToken =
    log.first_token_duration > 0
      ? formatMilliseconds(log.first_token_duration / 1_000_000)
      : "-";
  // 请求进行中使用同一份服务端快照中的字符数和流式传输时长, 结束后改用最终 Token 数。
  const outputCount = requestActive
    ? log.output_chars
    : log.usage.completion_tokens;
  const outputSpeed = responseMs > 0 ? outputCount / (responseMs / 1000) : 0;
  const outputSpeedUnit = requestActive ? "c/s" : "t/s";
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
      key: "apiKey",
      Icon: KeyRound,
      iconClassName: "size-3.5 shrink-0 text-orange-500",
      value: log.api_key_name || "-",
      cellClassName: "col-span-4 whitespace-nowrap md:col-span-1",
    },
    {
      key: "firstToken",
      Icon: Timer,
      iconClassName: "size-3.5 shrink-0 text-amber-500",
      value: firstToken,
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
      value: `${cachedTokens.toLocaleString()} (${cacheRate}%)`,
      cellClassName: "col-span-3 md:col-span-1",
    },
    {
      key: "completion",
      Icon: ArrowUpFromLine,
      iconClassName: "size-3.5 shrink-0 text-purple-500",
      value: outputCount.toLocaleString(),
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
    {
      key: "speed",
      Icon: Gauge,
      iconClassName: "size-3.5 shrink-0 text-sky-500",
      value:
        outputSpeed > 0 ? `${outputSpeed.toFixed(0)}${outputSpeedUnit}` : "-",
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
