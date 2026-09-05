import { useState } from 'react';
import { Ban, Loader2, RotateCcw, X } from 'lucide-react';
import { useTranslations } from 'use-intl';
import { type Group, type GroupItem } from '@/api/group';
import { getModelIcon } from '@/lib/model-icons';
import { cn } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { IconButton } from '@/components/common/IconButton';
import { MemberStatus } from '@/components/modules/group/MemberStatus';

interface LogGroupMemberRowProps {
    group: Group;
    item: GroupItem;
    now: number;
    itemCurrent: boolean;
    manualSelectionEnabled: boolean;
    routingBusy: boolean;
    togglePending: boolean;
    onSelect: (item: GroupItem) => void;
    onToggle: (item: GroupItem, enabled: boolean) => void;
}

// LogGroupMemberRow 渲染日志详情中的一个分组成员, 并提供当前分组范围内的排除与恢复。
export function LogGroupMemberRow({
    group,
    item,
    now,
    itemCurrent,
    manualSelectionEnabled,
    routingBusy,
    togglePending,
    onSelect,
    onToggle,
}: LogGroupMemberRowProps) {
    const t = useTranslations('log.card');
    const [confirming, setConfirming] = useState(false);
    const { Icon: ItemIcon, className: itemIconClassName } = getModelIcon(item.model_name);
    const routableCount = group.items.filter((entry) => entry.enabled && entry.available).length;
    const lastRoutable = item.enabled && item.available && routableCount === 1;
    const selectionDisabled = !manualSelectionEnabled || !item.enabled || routingBusy;

    const requestExclude = () => {
        if (routingBusy) return;
        if (lastRoutable) {
            setConfirming(true);
            return;
        }
        onToggle(item, false);
    };

    return (
        <div className="relative">
            <div className="flex w-full items-center">
                <button
                    type="button"
                    aria-pressed={itemCurrent}
                    disabled={selectionDisabled}
                    tabIndex={confirming ? -1 : undefined}
                    onClick={() => onSelect(item)}
                    className={cn(
                        'flex min-w-0 flex-1 items-center gap-2.5 rounded-lg px-3 py-2.5 text-left text-xs transition-colors hover:bg-muted/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 disabled:cursor-default disabled:hover:bg-transparent',
                        !item.enabled && 'opacity-60 grayscale',
                    )}
                >
                    <ItemIcon aria-hidden="true" className={itemIconClassName} width={20} height={20} />
                    <span className="min-w-0 flex-1">
                        <span className="block truncate font-semibold text-foreground">
                            {item.model_name}
                        </span>
                        <span className="block truncate text-[11px] text-muted-foreground">
                            {item.key_name ? `${item.channel_name} · ${item.key_name}` : item.channel_name}
                        </span>
                    </span>
                    {item.enabled ? (
                        <MemberStatus group={group} itemId={item.id} now={now} active={itemCurrent} />
                    ) : (
                        <Badge variant="outline" className="shrink-0 border-border bg-muted/50 px-1.5 py-0 text-[10px] font-medium text-muted-foreground">
                            {t('excluded')}
                        </Badge>
                    )}
                </button>

                <IconButton
                    tip={item.enabled ? t('excludeTooltip') : t('restoreTooltip')}
                    aria-label={item.enabled ? t('excludeMember', { name: item.model_name }) : t('restoreMember', { name: item.model_name })}
                    disabled={routingBusy}
                    tabIndex={confirming ? -1 : undefined}
                    onClick={(event) => {
                        event.stopPropagation();
                        if (item.enabled) requestExclude();
                        else onToggle(item, true);
                    }}
                    className={cn(
                        'mr-2 h-7 shrink-0 gap-1 rounded-md px-1.5 text-[11px] font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50',
                        item.enabled
                            ? 'hover:bg-destructive/10 hover:text-destructive'
                            : 'hover:bg-primary/10 hover:text-primary',
                    )}
                >
                    {togglePending ? (
                        <Loader2 className="size-3.5 animate-spin" />
                    ) : item.enabled ? (
                        <Ban className="size-3.5" />
                    ) : (
                        <RotateCcw className="size-3.5" />
                    )}
                    <span>{item.enabled ? t('exclude') : t('restore')}</span>
                </IconButton>
            </div>

            {confirming && (
                <div
                    role="alertdialog"
                    aria-label={t('excludeLastWarning')}
                    className="absolute inset-0 z-10 flex items-center gap-2 rounded-lg bg-destructive p-1.5 text-destructive-foreground"
                    onClick={(event) => event.stopPropagation()}
                    onKeyDown={(event) => {
                        if (event.key !== 'Escape') return;
                        event.stopPropagation();
                        setConfirming(false);
                    }}
                >
                    <span className="min-w-0 flex-1 px-1 text-[10px] font-medium leading-tight">
                        {t('excludeLastWarning')}
                    </span>
                    <button
                        type="button"
                        autoFocus
                        aria-label={t('cancel')}
                        onClick={(event) => {
                            event.stopPropagation();
                            setConfirming(false);
                        }}
                        className="flex size-6 shrink-0 items-center justify-center rounded-md bg-destructive-foreground/20 text-destructive-foreground transition-all hover:bg-destructive-foreground/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-destructive-foreground/60 active:scale-95"
                    >
                        <X className="size-3" />
                    </button>
                    <button
                        type="button"
                        onClick={(event) => {
                            event.stopPropagation();
                            setConfirming(false);
                            onToggle(item, false);
                        }}
                        disabled={routingBusy}
                        className="flex h-6 shrink-0 items-center gap-1 rounded-md bg-destructive-foreground px-2 text-[11px] font-semibold text-destructive transition-all hover:bg-destructive-foreground/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-destructive-foreground/60 active:scale-[0.98] disabled:opacity-50"
                    >
                        <Ban className="size-3" />
                        {t('confirmExclude')}
                    </button>
                </div>
            )}
        </div>
    );
}
