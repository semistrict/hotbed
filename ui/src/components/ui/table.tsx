"use client"

import * as React from "react"
import { Search, X } from "lucide-react"
import { cn } from "cn"
import { Input } from "./input"

export type TableFilterValue = string | number | bigint | null | undefined | false

const useIsomorphicLayoutEffect =
  typeof window !== "undefined" ? React.useLayoutEffect : React.useEffect

/**
 * Reports whether a filter string matches any of the given column values
 * (case-insensitive substring match; empty/whitespace queries match all rows).
 */
function matchesTableFilter(query: string, ...values: TableFilterValue[]): boolean {
  const needle = query.trim().toLowerCase()
  if (!needle) return true
  const haystack = values
    .filter((v): v is string | number | bigint => v !== null && v !== undefined && v !== false && v !== "")
    .map(String)
    .join("\n")
    .toLowerCase()
  return haystack.includes(needle)
}

/** Filters rows using a case-insensitive substring match across any column value. */
function filterTableRows<T>(
  rows: readonly T[],
  query: string,
  getValues: (row: T) => TableFilterValue[],
): T[] {
  const needle = query.trim().toLowerCase()
  if (!needle) return rows as T[]
  return rows.filter((row) => matchesTableFilter(needle, ...getValues(row)))
}

function rowSearchText(tr: HTMLTableRowElement): string {
  const parts: string[] = []
  for (const cell of Array.from(tr.cells)) {
    const text = cell.textContent?.trim()
    if (text) parts.push(text)
    const ownTitle = cell.getAttribute("title")?.trim()
    if (ownTitle) parts.push(ownTitle)
    const ownLabel = cell.getAttribute("aria-label")?.trim()
    if (ownLabel) parts.push(ownLabel)
    for (const el of Array.from(cell.querySelectorAll("[title],[aria-label]"))) {
      const t = el.getAttribute("title")?.trim()
      if (t) parts.push(t)
      const a = el.getAttribute("aria-label")?.trim()
      if (a) parts.push(a)
    }
  }
  return parts.join("\n").toLowerCase()
}

function TableFilterBar({
  value,
  onChange,
  placeholder = "Filter rows…",
  ariaLabel,
  shown,
  total,
  className,
}: {
  value: string
  onChange: (value: string) => void
  placeholder?: string
  ariaLabel?: string
  shown?: number
  total?: number
  className?: string
}) {
  const active = value.trim() !== ""
  return (
    <div data-slot="table-filter" className={cn("mb-2.5 flex flex-wrap items-center gap-2", className)}>
      <div className="relative w-full sm:max-w-xs">
        <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-faint" />
        <Input
          type="search"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
          aria-label={ariaLabel ?? placeholder}
          className="h-7.5 w-full rounded-lg bg-background/50 pr-7 pl-8 font-mono text-[11px] placeholder:text-faint"
        />
        {value && (
          <button
            type="button"
            onClick={() => onChange("")}
            aria-label="Clear filter"
            className="absolute top-1/2 right-2 -translate-y-1/2 text-faint hover:text-foreground"
          >
            <X className="size-3.5" />
          </button>
        )}
      </div>
      {active && shown !== undefined && total !== undefined && (
        <span className="num font-mono text-[10.5px] text-faint">
          {shown} of {total}
        </span>
      )}
    </div>
  )
}

export type TableProps = React.ComponentProps<"table"> & {
  /** Show a generic filter input above the table that matches any column (default: true). */
  filterable?: boolean
  filterPlaceholder?: string
  filterClassName?: string
  containerClassName?: string
}

function Table({
  className,
  filterable = true,
  filterPlaceholder = "Filter rows…",
  filterClassName,
  containerClassName,
  children,
  ...props
}: TableProps) {
  const [query, setQuery] = React.useState("")
  const [counts, setCounts] = React.useState<{ shown: number; total: number } | null>(null)
  const tableRef = React.useRef<HTMLTableElement>(null)
  const needle = query.trim().toLowerCase()

  useIsomorphicLayoutEffect(() => {
    if (!filterable) return
    const table = tableRef.current
    if (!table) return
    const rows = Array.from(table.querySelectorAll("tbody > tr")).filter(
      (tr): tr is HTMLTableRowElement =>
        tr instanceof HTMLTableRowElement && !tr.hasAttribute("data-table-filter-empty"),
    )
    if (!needle) {
      for (const tr of rows) tr.hidden = false
      setCounts((prev) => (prev === null ? prev : null))
      return
    }
    let shown = 0
    for (const tr of rows) {
      const match = rowSearchText(tr).includes(needle)
      tr.hidden = !match
      if (match) shown++
    }
    const total = rows.length
    setCounts((prev) => (prev && prev.shown === shown && prev.total === total ? prev : { shown, total }))
  }, [filterable, needle, children])

  const tableBox = (
    <div
      data-slot="table-container"
      className={cn("relative w-full overflow-x-auto", containerClassName)}
    >
      <table
        ref={tableRef}
        data-slot="table"
        className={cn("w-full caption-bottom text-sm", className)}
        {...props}
      >
        {children}
      </table>
      {filterable && counts && counts.total > 0 && counts.shown === 0 && (
        <p className="py-5 text-center font-mono text-[11px] text-muted-foreground">
          No rows match “{query.trim()}”.
        </p>
      )}
    </div>
  )

  if (!filterable) return tableBox

  return (
    <div data-slot="filterable-table">
      <TableFilterBar
        value={query}
        onChange={setQuery}
        placeholder={filterPlaceholder}
        shown={counts?.shown}
        total={counts?.total}
        className={filterClassName}
      />
      {tableBox}
    </div>
  )
}

function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return (
    <thead
      data-slot="table-header"
      className={cn("[&_tr]:border-b", className)}
      {...props}
    />
  )
}

function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return (
    <tbody
      data-slot="table-body"
      className={cn("[&_tr:last-child]:border-0", className)}
      {...props}
    />
  )
}

function TableFooter({ className, ...props }: React.ComponentProps<"tfoot">) {
  return (
    <tfoot
      data-slot="table-footer"
      className={cn(
        "border-t bg-muted/50 font-medium [&>tr]:last:border-b-0",
        className
      )}
      {...props}
    />
  )
}

function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return (
    <tr
      data-slot="table-row"
      className={cn(
        "border-b transition-colors hover:bg-muted/50 has-aria-expanded:bg-muted/50 data-[state=selected]:bg-muted",
        className
      )}
      {...props}
    />
  )
}

function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      data-slot="table-head"
      className={cn(
        "h-10 px-2 text-left align-middle font-medium whitespace-nowrap text-foreground [&:has([role=checkbox])]:pr-0",
        className
      )}
      {...props}
    />
  )
}

function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return (
    <td
      data-slot="table-cell"
      className={cn(
        "p-2 align-middle whitespace-nowrap [&:has([role=checkbox])]:pr-0",
        className
      )}
      {...props}
    />
  )
}

function TableCaption({
  className,
  ...props
}: React.ComponentProps<"caption">) {
  return (
    <caption
      data-slot="table-caption"
      className={cn("mt-4 text-sm text-muted-foreground", className)}
      {...props}
    />
  )
}

export {
  Table,
  TableHeader,
  TableBody,
  TableFooter,
  TableHead,
  TableRow,
  TableCell,
  TableCaption,
  TableFilterBar,
  matchesTableFilter,
  filterTableRows,
}
