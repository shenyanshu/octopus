import { useMemo } from "react";
import { ArrowUpAZ } from "lucide-react";
import { useTranslations } from "use-intl";
import { useModelList } from "@/api/model";
import {
  PageActions,
  usePageActionsStore,
} from "@/components/common/PageActions";
import { ModelItem } from "./Item";
import { CreateDialogContent } from "./Create";
import { VirtualizedGrid } from "@/components/common/VirtualizedGrid";

// ModelActions 向稳定顶栏提供模型页面的搜索、视图选项和创建入口。
export function ModelActions() {
  const t = useTranslations("toolbar");
  const searchTerm = usePageActionsStore(
    (state) => state.searchTerms.model || "",
  );
  const layout = usePageActionsStore((state) => state.layouts.model || "grid");
  const sortOrder = usePageActionsStore((state) =>
    state.sortOrders.model === "desc" ? "desc" : "asc",
  );
  const filter = usePageActionsStore((state) => state.modelFilter);
  const setSearchTerm = usePageActionsStore((state) => state.setSearchTerm);
  const setLayout = usePageActionsStore((state) => state.setLayout);
  const setSort = usePageActionsStore((state) => state.setSort);
  const setFilter = usePageActionsStore((state) => state.setModelFilter);

  return (
    <PageActions
      searchTerm={searchTerm}
      onSearchTermChange={(value) => setSearchTerm("model", value)}
      layout={layout}
      onLayoutChange={(value) => setLayout("model", value)}
      sortOptions={[
        { value: "asc", label: t("popover.nameAsc"), icon: ArrowUpAZ },
        { value: "desc", label: t("popover.nameDesc"), icon: ArrowUpAZ },
      ]}
      sortValue={sortOrder}
      onSortChange={(value) => {
        if (value === "asc" || value === "desc") setSort("model", value);
      }}
      filterOptions={[
        { value: "all", label: t("popover.filter.model.all") },
        { value: "priced", label: t("popover.filter.model.priced") },
        { value: "free", label: t("popover.filter.model.free") },
      ]}
      filterValue={filter}
      onFilterChange={(value) => {
        if (value === "all" || value === "priced" || value === "free")
          setFilter(value);
      }}
    >
      <CreateDialogContent />
    </PageActions>
  );
}

// Model 渲染模型列表正文。
export function Model() {
  const { data: models } = useModelList();
  const searchTerm = usePageActionsStore(
    (state) => state.searchTerms.model || "",
  );
  const layout = usePageActionsStore((state) => state.layouts.model || "grid");
  const sortOrder = usePageActionsStore((state) =>
    state.sortOrders.model === "desc" ? "desc" : "asc",
  );
  const filter = usePageActionsStore((state) => state.modelFilter);

  const sortedModels = useMemo(() => {
    if (!models) return [];
    return [...models].sort((a, b) =>
      sortOrder === "asc"
        ? a.name.localeCompare(b.name)
        : b.name.localeCompare(a.name),
    );
  }, [models, sortOrder]);

  const visibleModels = useMemo(() => {
    const term = searchTerm.toLowerCase().trim();
    const byName = !term
      ? sortedModels
      : sortedModels.filter((m) => m.name.toLowerCase().includes(term));
    // 契约把"免费"收窄到"已知价格的四项全 0", unknown(price null/price_known false)既非收费也非免费。
    const isFree = (model: (typeof byName)[number]) =>
      model.price_known &&
      model.price !== null &&
      model.price.input +
        model.price.output +
        model.price.cache_read +
        model.price.cache_write ===
        0;
    const isPriced = (model: (typeof byName)[number]) =>
      model.price_known && model.price !== null && !isFree(model);

    if (filter === "priced") {
      return byName.filter(isPriced);
    }
    if (filter === "free") {
      return byName.filter(isFree);
    }

    return byName;
  }, [sortedModels, searchTerm, filter]);

  return (
    <VirtualizedGrid
      items={visibleModels}
      layout={layout}
      columns={{ default: 1, md: 2, lg: 3 }}
      estimateItemHeight={112}
      getItemKey={(model) => `model-${model.name}`}
      renderItem={(model) => <ModelItem model={model} layout={layout} />}
    />
  );
}
