import { useTranslations } from 'use-intl';
import { Info, Tag } from 'lucide-react';
import Github from '@thesvg/react/github';

const APP_VERSION = import.meta.env.VITE_APP_VERSION || ''; // 当前前端构建对应的应用版本。
const GITHUB_REPO = import.meta.env.VITE_GITHUB_REPO || 'https://github.com/shenyanshu/octopus'; // 项目仓库地址。

// SettingInfo 仅展示项目基本信息；本分支只发布 Docker 镜像，不再提供二进制自动更新能力。
export function SettingInfo() {
    const t = useTranslations('setting');

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                <Info className="h-5 w-5" />
                {t('info.title')}
            </h2>
            {/* GitHub 仓库 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Github variant="mono" className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.github')}</span>
                </div>
                <a
                    href={GITHUB_REPO}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-sm text-primary hover:underline"
                >
                    {GITHUB_REPO.replace('https://github.com/', '')}
                </a>
            </div>
            {/* 前端构建版本，静态展示，不依赖更新接口 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Tag className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.currentVersion')}</span>
                </div>
                <code className="text-sm font-mono text-muted-foreground">
                    {APP_VERSION || t('info.unknown')}
                </code>
            </div>
        </div>
    );
}
