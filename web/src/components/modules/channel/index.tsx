import { useMemo } from "react";
import { ArrowUpAZ, RefreshCw } from "lucide-react";
import { useTranslations } from "use-intl";
import { toast } from "sonner";
import {
  useChannelStats,
  useChannelSyncCompletionInvalidation,
  useChannelSyncStatus,
  useSyncAllChannelModels,
} from "@/api/channel";
import { classifyBatchSyncResult, hasRunningSync } from "@/api/channel-sync";
import {
  PageActions,
  usePageActionsStore,
} from "@/components/common/PageActions";
import { MorphingDialogDescription } from "@/components/ui/morphing-dialog";
import { buttonVariants } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { Card } from "./Card";
import { ChannelForm } from "./Form";
import { VirtualizedGrid } from "@/components/common/VirtualizedGrid";

// ChannelActions 向稳定顶栏提供渠道页面的搜索、视图选项、批量同步和创建入口。
export function ChannelActions() {
  const t = useTranslations("toolbar");
  const tSync = useTranslations("channel.sync");
  const syncAll = useSyncAllChannelModels();
  const { data: syncStatuses } = useChannelSyncStatus();
  // 有同步在跑或请求未返回时禁用, 避免连点; 后端也会去重, 这里是第一层。
  const syncRunning = hasRunningSync(syncStatuses);

  // 批量只作用于"已启用且开启自动同步"的渠道, 回执按启动/进行中去重/跳过分类提示, 不谎报完成。
  const handleSyncAll = () => {
    syncAll.mutate(undefined, {
      onSuccess: (result) => {
        const outcome = classifyBatchSyncResult(result);
        if (outcome.kind === "started")
          toast.success(tSync("batchStarted", { count: outcome.count }));
        else if (outcome.kind === "busy")
          toast.info(tSync("batchBusy", { count: outcome.count }));
        else toast.info(tSync("batchNone"));
      },
      onError: (error) => toast.error(error.message),
    });
  };
  const searchTerm = usePageActionsStore(
    (state) => state.searchTerms.channel || "",
  );
  const layout = usePageActionsStore(
    (state) => state.layouts.channel || "grid",
  );
  const sortOrder = usePageActionsStore((state) =>
    state.sortOrders.channel === "desc" ? "desc" : "asc",
  );
  const filter = usePageActionsStore((state) => state.channelFilter);
  const setSearchTerm = usePageActionsStore((state) => state.setSearchTerm);
  const setLayout = usePageActionsStore((state) => state.setLayout);
  const setSort = usePageActionsStore((state) => state.setSort);
  const setFilter = usePageActionsStore((state) => state.setChannelFilter);

  return (
    <div className="flex items-center gap-2">
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            onClick={handleSyncAll}
            disabled={syncAll.isPending || syncRunning}
            aria-label={tSync("batchSync")}
            className={buttonVariants({
              variant: "ghost",
              size: "icon",
              className:
                "rounded-xl transition-none hover:bg-transparent text-muted-foreground hover:text-foreground",
            })}
          >
            <RefreshCw
              className={`size-4 transition-colors duration-300 ${syncRunning ? "animate-spin" : ""}`}
            />
          </button>
        </TooltipTrigger>
        <TooltipContent side="bottom" sideOffset={10} align="center">
          {tSync("batchSyncHint")}
        </TooltipContent>
      </Tooltip>
      <PageActions
        searchTerm={searchTerm}
        onSearchTermChange={(value) => setSearchTerm("channel", value)}
        layout={layout}
        onLayoutChange={(value) => setLayout("channel", value)}
        sortOptions={[
          { value: "asc", label: t("popover.nameAsc"), icon: ArrowUpAZ },
          { value: "desc", label: t("popover.nameDesc"), icon: ArrowUpAZ },
        ]}
        sortValue={sortOrder}
        onSortChange={(value) => {
          if (value === "asc" || value === "desc") setSort("channel", value);
        }}
        filterOptions={[
          { value: "all", label: t("popover.filter.channel.all") },
          { value: "enabled", label: t("popover.filter.channel.enabled") },
          { value: "disabled", label: t("popover.filter.channel.disabled") },
        ]}
        filterValue={filter}
        onFilterChange={(value) => {
          if (value === "all" || value === "enabled" || value === "disabled")
            setFilter(value);
        }}
      >
        <div className="w-screen max-w-full md:max-w-3xl flex flex-col">
          <MorphingDialogDescription disableLayoutAnimation>
            <ChannelForm />
          </MorphingDialogDescription>
        </div>
      </PageActions>
    </div>
  );
}

// Channel 渲染渠道列表正文。
export function Channel() {
  const { data: statsData } = useChannelStats();
  // 页面展示期间订阅同步状态(空闲 30s / 有运行 2s), 并在同步落入终态时失效相关查询。
  const { data: syncStatuses } = useChannelSyncStatus();
  useChannelSyncCompletionInvalidation(syncStatuses);
  const searchTerm = usePageActionsStore(
    (state) => state.searchTerms.channel || "",
  );
  const layout = usePageActionsStore(
    (state) => state.layouts.channel || "grid",
  );
  const sortOrder = usePageActionsStore((state) =>
    state.sortOrders.channel === "desc" ? "desc" : "asc",
  );
  const filter = usePageActionsStore((state) => state.channelFilter);

  // 先按搜索词和启用状态过滤, 再按名称排序
  const visibleChannels = useMemo(() => {
    const term = searchTerm.toLowerCase().trim();
    const matched = (statsData ?? []).filter((channel) => {
      if (term && !channel.channel_name.toLowerCase().includes(term))
        return false;
      if (filter === "enabled") return channel.enabled;
      if (filter === "disabled") return !channel.enabled;
      return true;
    });

    return matched.sort((a, b) =>
      sortOrder === "asc"
        ? a.channel_name.localeCompare(b.channel_name)
        : b.channel_name.localeCompare(a.channel_name),
    );
  }, [statsData, searchTerm, filter, sortOrder]);

  return (
    <VirtualizedGrid
      items={visibleChannels}
      layout={layout}
      columns={{ default: 1, sm: 2, md: 3, lg: 4, xl: 5, "2xl": 6 }}
      estimateItemHeight={232}
      getItemKey={(channel) => `channel-${channel.channel_id}`}
      renderItem={(channel) => <Card channel={channel} />}
    />
  );
}
