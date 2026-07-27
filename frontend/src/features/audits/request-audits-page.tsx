import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowDown, ArrowUp, BrainCircuit, CircleCheck, CircleDollarSign, Database, Info, RefreshCw, Search, WholeWord, type LucideIcon } from "lucide-react";
import { memo, useCallback, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import { RequestAuditDetailDialog } from "@/features/audits/request-audit-detail-dialog";
import { getRequestAudits, getRequestAuditSummary, type AuditDTO, type AuditPeriod } from "@/features/audits/request-audits-api";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { CursorPagination } from "@/shared/components/pagination";
import { PageHeader } from "@/shared/components/page-header";
import { PeriodSelector } from "@/shared/components/period-selector";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatDuration, formatNumber } from "@/shared/lib/format";
import { toPeriodValue, type PeriodDays } from "@/shared/lib/period";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const AUDIT_PAGE_CACHE_TIME_MS = 60_000;
const AUDIT_SUMMARY_CACHE_TIME_MS = 120_000;

type AuditCursorState = { scope: string; values: string[] };

export function RequestAuditsPage() {
  const { t, i18n } = useTranslation();
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [modelFilter, setModelFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [modeFilter, setModeFilter] = useState("");
  const [keyFilter, setKeyFilter] = useState("");
  const [accountFilter, setAccountFilter] = useState("");
  const [periodDays, setPeriodDays] = useState<PeriodDays>(1);
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const [selectedAudit, setSelectedAudit] = useState<AuditDTO | null>(null);
  const forceSummaryRefresh = useRef(false);
  const debouncedSearch = useDebouncedValue(search);
  const debouncedKeyFilter = useDebouncedValue(keyFilter);
  const debouncedAccountFilter = useDebouncedValue(accountFilter);
  const period: AuditPeriod = toPeriodValue(periodDays);
  const cursorScope = useMemo(() => JSON.stringify([
    pageSize, debouncedSearch, modelFilter, statusFilter, modeFilter,
    debouncedKeyFilter, debouncedAccountFilter, period, sort.field, sort.order,
  ]), [pageSize, debouncedSearch, modelFilter, statusFilter, modeFilter, debouncedKeyFilter, debouncedAccountFilter, period, sort.field, sort.order]);
  const [cursorState, setCursorState] = useState<AuditCursorState>(() => ({ scope: cursorScope, values: [""] }));
  if (cursorState.scope !== cursorScope) {
    setCursorState({ scope: cursorScope, values: [""] });
  }
  const cursors = cursorState.scope === cursorScope ? cursorState.values : [""];
  const cursor = cursors[cursors.length - 1];

  const updateCursors = useCallback((update: (values: string[]) => string[]) => {
    setCursorState((current) => {
      const values = current.scope === cursorScope ? current.values : [""];
      return { scope: cursorScope, values: update(values) };
    });
  }, [cursorScope]);

  const auditsQuery = useQuery({
    queryKey: ["request-audits", "cursor", cursorScope, cursor],
    queryFn: ({ signal }) => getRequestAudits({ cursor, pageSize, search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period, sortBy: sort.field, sortOrder: sort.order }, signal),
    placeholderData: (previous, previousQuery) => previousQuery?.queryKey[2] === cursorScope ? previous : undefined,
    gcTime: AUDIT_PAGE_CACHE_TIME_MS,
    structuralSharing: false,
  });
  const summaryQuery = useQuery({
    queryKey: ["request-audits", "summary", debouncedSearch, modelFilter, statusFilter, modeFilter, debouncedKeyFilter, debouncedAccountFilter, period],
    queryFn: ({ signal }) => getRequestAuditSummary({ search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period }, forceSummaryRefresh.current, signal),
    placeholderData: (previous) => previous,
    gcTime: AUDIT_SUMMARY_CACHE_TIME_MS,
  });
  const modelOptionsQuery = useQuery({
    queryKey: ["models", "audit-filter"],
    queryFn: () => listModels({ page: 1, pageSize: 100 }),
    staleTime: 60_000,
  });
  const result = auditsQuery.data;
  const nextCursor = result?.nextCursor ?? "";
  const summary = summaryQuery.data;
  const summaryLoading = summaryQuery.isPending || summaryQuery.isPlaceholderData;
  const cacheRate = summary?.usage.inputTokens ? summary.usage.cachedInputTokens / summary.usage.inputTokens * 100 : 0;
  const estimatedCostTicks = summary?.usage.estimatedCostInUsdTicks ?? 0;
  const hasEstimatedCost = (summary?.pricing.pricedRequests ?? 0) > 0;
  const modelOptions = useMemo(() => [...new Map((modelOptionsQuery.data?.items ?? []).map((model) => [model.publicId, { value: model.publicId, label: model.publicId }])).values()], [modelOptionsQuery.data?.items]);
  const openAudit = useCallback((audit: AuditDTO) => setSelectedAudit(audit), []);
  const renderAuditRow = useCallback((audit: AuditDTO) => <AuditRow key={audit.id} audit={audit} locale={i18n.language} onOpen={openAudit} />, [i18n.language, openAudit]);

  function refreshAll(): void {
    setManualRefreshing(true);
    forceSummaryRefresh.current = true;
    void Promise.all([
      auditsQuery.refetch(),
      summaryQuery.refetch(),
      new Promise<void>((resolve) => window.setTimeout(resolve, 400)),
    ]).finally(() => {
      forceSummaryRefresh.current = false;
      setManualRefreshing(false);
    });
  }

  const changeSort = useCallback((field: string, initialOrder: SortOrder): void => {
    setSort((current) => nextTableSort(current, field, initialOrder));
  }, []);

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("audits.title")}
        description={t("audits.description")}
        actions={(
          <>
            <PeriodSelector value={periodDays} onChange={setPeriodDays} ariaLabel={t("audits.usageSummary")} />
            <Button variant="secondary" size="sm" onClick={refreshAll} disabled={auditsQuery.isFetching || summaryQuery.isFetching || manualRefreshing}><RefreshCw className={manualRefreshing || auditsQuery.isFetching || summaryQuery.isFetching ? "animate-spin" : undefined} />{t("common.refresh")}</Button>
          </>
        )}
      />

      <section className="space-y-2" aria-label={t("audits.usageSummary")}>
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
          <AuditMetric icon={Activity} loading={summaryLoading} label={t("audits.totalRequests")} value={formatNumber(summary?.usage.requests ?? 0, i18n.language, 0)} detail={t("audits.requestBreakdown", { success: formatNumber(summary?.usage.successfulRequests ?? 0, i18n.language, 0), failed: formatNumber(summary?.usage.failedRequests ?? 0, i18n.language, 0) })} />
          <AuditMetric icon={WholeWord} loading={summaryLoading} label={t("audits.totalTokens")} value={formatNumber(summary?.usage.totalTokens ?? 0, i18n.language, 0)} detail={t("audits.tokenEfficiency", { cacheRate: formatNumber(cacheRate, i18n.language, 1) })} />
          <AuditMetric icon={CircleCheck} loading={summaryLoading} label={t("audits.successRate")} value={`${formatNumber(summary?.usage.successRate ?? 0, i18n.language, 1)}%`} detail={t("audits.averageDuration", { duration: formatDuration(summary?.usage.averageDurationMs ?? 0) })} />
          <AuditMetric
            icon={CircleDollarSign}
            loading={summaryLoading}
            label={t("audits.estimatedCost")}
            value={hasEstimatedCost ? formatUSDCost(estimatedCostTicks, 2) : "-"}
            fullValue={hasEstimatedCost ? formatUSDCost(estimatedCostTicks, 10) : undefined}
            detail={t("audits.pricingCoverage", { priced: formatNumber(summary?.pricing.pricedRequests ?? 0, i18n.language, 0), unpriced: formatNumber(summary?.pricing.unpricedRequests ?? 0, i18n.language, 0) })}
            tooltip={t("audits.pricingDescription")}
          />
        </div>
        <div className="grid grid-cols-2 gap-2 xl:grid-cols-4">
          <AuditTokenMetric icon={ArrowUp} loading={summaryLoading} label={t("audits.input")} value={formatNumber(summary?.usage.inputTokens ?? 0, i18n.language, 0)} />
          <AuditTokenMetric icon={ArrowDown} loading={summaryLoading} label={t("audits.output")} value={formatNumber(summary?.usage.outputTokens ?? 0, i18n.language, 0)} />
          <AuditTokenMetric icon={Database} loading={summaryLoading} label={t("audits.cached")} value={formatNumber(summary?.usage.cachedInputTokens ?? 0, i18n.language, 0)} />
          <AuditTokenMetric icon={BrainCircuit} loading={summaryLoading} label={t("audits.reasoning")} value={formatNumber(summary?.usage.reasoningTokens ?? 0, i18n.language, 0)} />
        </div>
      </section>

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("audits.search")} aria-label={t("audits.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "model", label: t("audits.model"), value: modelFilter, onChange: setModelFilter, options: modelOptions },
                { id: "status", label: t("audits.status"), value: statusFilter, onChange: setStatusFilter, options: [
                  { value: "issues", label: t("audits.statusIssues") },
                  { value: "2xx", label: `2xx · ${t("audits.statusSuccess")}` },
                  { value: "4xx", label: `4xx · ${t("audits.statusClientError")}` },
                  { value: "5xx", label: `5xx · ${t("audits.statusServerError")}` },
                ] },
                { id: "mode", label: t("audits.mode"), value: modeFilter, onChange: setModeFilter, options: [
                  { value: "stream", label: t("audits.stream") },
                  { value: "nonStream", label: t("audits.nonStream") },
                ] },
                { id: "key", type: "text", label: t("audits.key"), value: keyFilter, placeholder: t("audits.keyFilterPlaceholder"), onChange: setKeyFilter },
                { id: "account", type: "text", label: t("audits.account"), value: accountFilter, placeholder: t("audits.accountFilterPlaceholder"), onChange: setAccountFilter },
              ]} />
            </div>
          </>
        )}
        footer={(result?.items.length ?? 0) > 0 || cursors.length > 1 ? (
          <CursorPagination
            page={cursors.length}
            pageSize={pageSize}
            hasMore={Boolean(result?.hasMore && nextCursor)}
            disabled={auditsQuery.isFetching}
            onFirstPage={() => updateCursors(() => [""])}
            onPreviousPage={() => updateCursors((values) => values.length > 1 ? values.slice(0, -1) : values)}
            onNextPage={() => { if (nextCursor) updateCursors((values) => [...values, nextCursor]); }}
            onPageSizeChange={setPageSize}
          />
        ) : undefined}
      >
        {auditsQuery.isError ? <ErrorState message={auditsQuery.error.message} onRetry={() => void auditsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {auditsQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={22} rowHeight={64} aria-busy={auditsQuery.isFetching} className={cn("min-w-[1680px] w-full table-fixed text-xs transition-opacity", auditsQuery.isPlaceholderData && "pointer-events-none opacity-60")}>
            <colgroup>
              <col className="w-[9%]" />
              <col className="w-[11%]" />
              <col className="w-[16%]" />
              <col className="w-[7%]" />
              <col className="w-[12%]" />
              <col className="w-[6%]" />
              <col className="w-[8%]" />
              <col className="w-[8%]" />
              <col className="w-[7%]" />
              <col className="w-[6%]" />
              <col className="w-[10%]" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.createdAt")}</SortableTableHead>
                <SortableTableHead field="request" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.request")}</SortableTableHead>
                <TableHead>{t("audits.account")}</TableHead>
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.model")}</SortableTableHead>
                <SortableTableHead field="tokens" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.tokens")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("audits.status")}</SortableTableHead>
                <SortableTableHead field="duration" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.duration")}</SortableTableHead>
                <SortableTableHead field="ttft" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.ttft")}</SortableTableHead>
                <SortableTableHead field="tps" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.tps")}</SortableTableHead>
                <TableHead className="text-center">{t("audits.upstreamAttempts")}</TableHead>
                <TableHead>{t("audits.egress")}</TableHead>
              </TableRow>
            </TableHeader>
            {auditsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={11} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={11} rowHeight={64} overscan={8} renderRow={renderAuditRow} />
            )}
          </Table>
        ) : null}
      </DataTableShell>
      <RequestAuditDetailDialog key={selectedAudit?.id ?? "closed"} audit={selectedAudit} open={selectedAudit !== null} onOpenChange={(open) => !open && setSelectedAudit(null)} />
    </div>
  );
}

const AuditRow = memo(function AuditRow({ audit, locale, onOpen }: { audit: AuditDTO; locale: string; onOpen: (audit: AuditDTO) => void }) {
  const ttft = audit.ttftMs ?? 0;
  const tps = audit.tokensPerSecond ?? 0;
  const attempts = Math.max(audit.upstreamAttempts ?? 0, audit.attemptCount ?? 0, 1);
  return (
    <TableRow className="h-[64px]">
      <TableCell className="whitespace-nowrap text-xs tabular-nums text-muted-foreground">{formatDateTime(audit.createdAt, locale)}</TableCell>
      <TableCell><RequestValue audit={audit} /></TableCell>
      <TableCell>
        <AccountValue
          account={audit.accountName || (audit.accountId ? `#${audit.accountId}` : "-")}
          accountId={audit.accountId}
          clientKey={audit.clientKeyName || `#${audit.clientKeyId}`}
          accountRequests={audit.accountRequestCount ?? 0}
          accountSuccesses={audit.accountSuccessCount ?? 0}
          accountFailures={audit.accountFailureCount ?? 0}
        />
      </TableCell>
      <TableCell>
        <div className="min-w-0">
          <span className="block truncate text-xs font-medium" title={audit.modelPublicId || undefined}>{audit.modelPublicId || `#${audit.modelRouteId}`}</span>
          <span className="mt-0.5 block truncate text-[10px] text-muted-foreground" title={audit.modelUpstreamModel || undefined}>{audit.modelUpstreamModel || "-"}</span>
        </div>
      </TableCell>
      <TableCell><TokenCompact audit={audit} locale={locale} /></TableCell>
      <TableCell className="text-center"><AuditStatus audit={audit} onOpen={() => onOpen(audit)} /></TableCell>
      <TableCell className="whitespace-nowrap text-xs font-medium tabular-nums">{formatDuration(audit.durationMs)}</TableCell>
      <TableCell className="whitespace-nowrap text-xs tabular-nums">
        {ttft > 0 ? formatDuration(ttft) : <span className="text-muted-foreground">-</span>}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs tabular-nums">
        {tps > 0 ? `${formatNumber(tps, locale, 1)} t/s` : <span className="text-muted-foreground">-</span>}
      </TableCell>
      <TableCell className="text-center text-xs tabular-nums"><AttemptsValue audit={audit} attempts={attempts} /></TableCell>
      <TableCell><EgressValue audit={audit} /></TableCell>
    </TableRow>
  );
});

function RequestValue({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  return (
    <div className="min-w-0">
      <span className="block truncate text-xs font-medium">{providerLabel(audit.provider)} · {t(`audits.operations.${audit.operation}`)}</span>
      <span className="mt-0.5 block truncate font-mono text-[10px] text-muted-foreground" title={audit.requestId}>{audit.requestId}</span>
    </div>
  );
}

function EgressValue({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  if (!audit.egressMode) {
    return <span className="text-muted-foreground">-</span>;
  }
  const proxied = audit.egressMode === "proxy";
  const node = audit.egressNodeName || (proxied ? t("audits.egressUnknown") : t("audits.egressDirect"));
  const details = [audit.egressScope, audit.egressNodeId ? `#${audit.egressNodeId}` : ""].filter(Boolean).join(" · ");
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="block min-w-0 max-w-full cursor-help text-left" aria-label={`${proxied ? t("audits.egressProxy") : t("audits.egressDirect")}: ${node}`}>
          <span className={cn("inline-flex items-center gap-1.5 text-xs", proxied ? "text-emerald-700 dark:text-emerald-300" : "text-muted-foreground")}>
            <span className={cn("size-1.5 rounded-full", proxied ? "bg-emerald-500" : "bg-muted-foreground/50")} />
            {proxied ? t("audits.egressProxy") : t("audits.egressDirect")}
          </span>
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-72" side="top" align="start">
        <div>{node}</div>
        {details ? <div className="mt-1 text-primary-foreground/65">{details}</div> : null}
      </TooltipContent>
    </Tooltip>
  );
}

function AuditMetric({ icon: Icon, label, value, detail, tooltip, fullValue, loading }: { icon: LucideIcon; label: string; value: string; detail?: string; tooltip?: string; fullValue?: string; loading: boolean }) {
  const { t } = useTranslation();
  return (
    <article className="min-h-28 rounded-lg bg-card p-4" aria-busy={loading}>
      <header className="flex min-h-5 items-center justify-between gap-3">
        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <span>{label}</span>
          {tooltip ? (
            <Tooltip>
              <TooltipTrigger asChild><button type="button" className="cursor-help" aria-label={tooltip}><Info className="size-3.5" /></button></TooltipTrigger>
              <TooltipContent className="max-w-72 leading-5">{tooltip}</TooltipContent>
            </Tooltip>
          ) : null}
        </div>
        <Icon className="size-4 shrink-0 text-muted-foreground" />
      </header>
      <div className="mt-3 flex min-h-8 items-center text-2xl font-medium tracking-tight tabular-nums">
        {loading ? <Spinner /> : fullValue ? (
          <Tooltip>
            <TooltipTrigger asChild><span className="cursor-help" tabIndex={0}>{value}</span></TooltipTrigger>
            <TooltipContent side="top"><span className="text-primary-foreground/65">{t("audits.exactBilling")}</span> <span className="font-mono">{fullValue}</span></TooltipContent>
          </Tooltip>
        ) : value}
      </div>
      {detail ? <p className={cn("mt-1.5 min-h-4 truncate text-[11px] text-muted-foreground", loading && "invisible")} title={detail}>{detail}</p> : null}
    </article>
  );
}

function AuditTokenMetric({ icon: Icon, label, value, loading }: { icon: LucideIcon; label: string; value: string; loading: boolean }) {
  return (
    <div className="flex min-h-11 min-w-0 items-center justify-between gap-3 rounded-lg bg-muted/45 px-4 py-2">
      <span className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground"><Icon className="size-3.5 shrink-0" />{label}</span>
      <span className="flex min-h-5 min-w-8 items-center justify-end truncate text-sm font-medium tabular-nums" title={loading ? undefined : value}>{loading ? <Spinner className="size-3.5" /> : value}</span>
    </div>
  );
}

function AccountValue({
  account, accountId, clientKey, accountRequests, accountSuccesses, accountFailures,
}: {
  account: string; accountId?: string; clientKey: string;
  accountRequests: number; accountSuccesses: number; accountFailures: number; locale?: string;
}) {
  const { t } = useTranslation();
  const hasStats = accountRequests > 0;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="block w-full min-w-0 cursor-help text-left" aria-label={t("audits.routeDetails")}>
          <span className="block truncate text-xs font-medium" title={account}>{account}</span>
          {hasStats ? (
            <span className="mt-0.5 block truncate text-[10px] tabular-nums text-muted-foreground">
              {t("audits.accountStatsAsOf", { success: accountSuccesses, failed: accountFailures, total: accountRequests })}
            </span>
          ) : (
            <span className="mt-0.5 block truncate text-[10px] text-muted-foreground">{clientKey}</span>
          )}
        </button>
      </TooltipTrigger>
      <TooltipContent className="w-80 space-y-1.5 py-2" side="top" align="start">
        <div className="grid grid-cols-[auto_1fr] gap-x-3">
          <span className="text-primary-foreground/65">{t("audits.owningAccount")}</span>
          <span className="truncate text-right" title={account}>{account}</span>
        </div>
        {accountId ? (
          <div className="grid grid-cols-[auto_1fr] gap-x-3">
            <span className="text-primary-foreground/65">ID</span>
            <span className="text-right font-mono tabular-nums">#{accountId}</span>
          </div>
        ) : null}
        <div className="grid grid-cols-[auto_1fr] gap-x-3">
          <span className="text-primary-foreground/65">{t("audits.accountStatsAsOfHint")}</span>
          <span className="text-right tabular-nums">{t("audits.accountStatsDetail", { total: accountRequests, success: accountSuccesses, failed: accountFailures })}</span>
        </div>
        <div className="grid grid-cols-[auto_1fr] gap-x-3">
          <span className="text-primary-foreground/65">{t("audits.owningKey")}</span>
          <span className="truncate text-right" title={clientKey}>{clientKey}</span>
        </div>
        <div className="text-[11px] text-primary-foreground/65">{t("audits.accountStatsAsOfNote")}</div>
      </TooltipContent>
    </Tooltip>
  );
}

function AttemptsValue({ audit, attempts }: { audit: AuditDTO; attempts: number }) {
  const { t } = useTranslation();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="cursor-help tabular-nums">{attempts}</button>
      </TooltipTrigger>
      <TooltipContent className="max-w-72 space-y-1 py-2" side="top">
        <div>{t("audits.upstreamAttempts")}: {audit.upstreamAttempts ?? 0}</div>
        <div>{t("audits.failedAttempts")}: {audit.attemptCount ?? 0}</div>
        <div>{t("audits.selection")}: {formatDuration(audit.selectionMs ?? 0)} · {t("audits.credential")}: {formatDuration(audit.credentialMs ?? 0)}</div>
        <div>{t("audits.firstHeaders")}: {formatDuration(audit.firstHeadersMs ?? 0)} · {t("audits.upstreamWait")}: {formatDuration(audit.upstreamWaitMs ?? 0)}</div>
      </TooltipContent>
    </Tooltip>
  );
}

function TokenCompact({ audit, locale }: { audit: AuditDTO; locale: string }) {
  const { t } = useTranslation();
  if (audit.operation === "image" || audit.operation === "image_edit") {
    return <span className="text-xs tabular-nums">{t("audits.imageCount", { count: audit.mediaOutputImages })}</span>;
  }
  if (audit.operation === "video") {
    return <span className="text-xs tabular-nums">{t("audits.secondsCount", { count: audit.mediaOutputSeconds })}</span>;
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="block min-w-0 max-w-full cursor-help text-left">
          <span className="block text-xs font-medium tabular-nums">{formatNumber(audit.totalTokens, locale, 0)}</span>
          <span className="mt-0.5 block truncate text-[10px] tabular-nums text-muted-foreground">
            ↑{formatNumber(audit.inputTokens, locale, 0)} ↓{formatNumber(audit.outputTokens, locale, 0)}
            {audit.reasoningTokens > 0 ? ` · R${formatNumber(audit.reasoningTokens, locale, 0)}` : ""}
            {audit.cachedInputTokens > 0 ? ` · C${formatNumber(audit.cachedInputTokens, locale, 0)}` : ""}
          </span>
        </button>
      </TooltipTrigger>
      <TooltipContent className="space-y-1 py-2" side="top">
        <div>{t("audits.input")}: {formatNumber(audit.inputTokens, locale, 0)}</div>
        <div>{t("audits.output")}: {formatNumber(audit.outputTokens, locale, 0)}</div>
        <div>{t("audits.cached")}: {formatNumber(audit.cachedInputTokens, locale, 0)}</div>
        <div>{t("audits.reasoning")}: {formatNumber(audit.reasoningTokens, locale, 0)}</div>
        <div>{t("audits.total")}: {formatNumber(audit.totalTokens, locale, 0)}</div>
      </TooltipContent>
    </Tooltip>
  );
}

function StatusCode({ statusCode, hasError = false }: { statusCode: number; hasError?: boolean }) {
  const tone = statusTone(statusCode, hasError);
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs tabular-nums", tone.text)}>
      <span className={cn("size-1.5 rounded-full", tone.dot)} />
      {statusCode || "-"}
    </span>
  );
}

function AuditStatus({ audit, onOpen }: { audit: AuditDTO; onOpen: () => void }) {
  const { t } = useTranslation();
  const mode = audit.operation === "compaction" ? t("audits.operations.compaction") : audit.streaming ? t("audits.stream") : t("audits.nonStream");
  // Soft failures (2xx + errorCode) and hard HTTP failures open the diagnostics dialog.
  const hasIssue = Boolean(audit.errorCode) || audit.attemptCount > 0 || audit.statusCode < 200 || audit.statusCode >= 300;
  const content = (
    <>
      <StatusCode statusCode={audit.statusCode} hasError={hasIssue} />
      <span className="block whitespace-nowrap text-[10px] text-muted-foreground">{mode}</span>
    </>
  );
  // Always clickable when there is anything to diagnose; keep the status column clean
  // and put the real error body / transport detail inside the dialog.
  if (!hasIssue) return <div className="space-y-0.5 text-center">{content}</div>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="group space-y-0.5 rounded-md text-center outline-none focus-visible:ring-2 focus-visible:ring-ring/50 [&>span:last-child]:underline-offset-2 hover:[&>span:last-child]:text-foreground hover:[&>span:last-child]:underline" aria-label={t("audits.openDiagnostics")} onClick={onOpen}>{content}</button>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 whitespace-normal break-words text-left leading-5" side="top">
        <div>{t("audits.clickForDiagnostics")}</div>
        {audit.errorCode ? <div className="mt-1 text-primary-foreground/70">{audit.errorCode}</div> : null}
      </TooltipContent>
    </Tooltip>
  );
}

function statusTone(statusCode: number, hasError = false): { dot: string; text: string } {
  if (hasError) return { dot: "bg-amber-500", text: "text-amber-700 dark:text-amber-300" };
  if (statusCode >= 500) return { dot: "bg-red-500", text: "text-red-700 dark:text-red-300" };
  if (statusCode >= 400) return { dot: "bg-amber-500", text: "text-amber-700 dark:text-amber-300" };
  if (statusCode >= 200 && statusCode < 300) return { dot: "bg-emerald-500", text: "text-emerald-700 dark:text-emerald-300" };
  return { dot: "bg-muted-foreground/50", text: "text-muted-foreground" };
}

function providerLabel(provider: AuditDTO["provider"]): string {
  switch (provider) {
    case "grok_build":
      return "Grok Build";
    case "grok_web":
      return "Grok Web";
    case "grok_console":
      return "Grok Console";
  }
}

function formatUSDCost(ticks: number, fractionDigits: number): string {
  return `$${(ticks / 10_000_000_000).toFixed(fractionDigits)}`;
}
