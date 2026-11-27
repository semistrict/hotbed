import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import {
  filterTableRows,
  matchesTableFilter,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "./table";

describe("Table generic filter", () => {
  it("matches case-insensitive substrings across any column without crossing column boundaries", () => {
    expect(matchesTableFilter("", "alpha", "beta")).toBe(true);
    expect(matchesTableFilter("   ", "alpha", "beta")).toBe(true);
    expect(matchesTableFilter("ALP", "alpha", "beta")).toBe(true);
    expect(matchesTableFilter("beta", "alpha", "beta")).toBe(true);
    expect(matchesTableFilter("42", "alpha", 42n)).toBe(true);
    expect(matchesTableFilter("hab", "alpha", "beta")).toBe(false);
  });

  it("filters row lists across arbitrary column extractors", () => {
    const rows = [
      { id: "c-1", host: "ubu1", argv: "go test ./..." },
      { id: "c-2", host: "asahimbp", argv: "cargo test" },
    ];
    expect(filterTableRows(rows, "c-2", (r) => [r.id, r.host, r.argv]).map((r) => r.id)).toEqual(["c-2"]);
    expect(filterTableRows(rows, "ubu1", (r) => [r.id, r.host, r.argv]).map((r) => r.id)).toEqual(["c-1"]);
    expect(filterTableRows(rows, "TEST", (r) => [r.id, r.host, r.argv]).map((r) => r.id)).toEqual(["c-1", "c-2"]);
    expect(filterTableRows(rows, "missing", (r) => [r.id, r.host, r.argv])).toEqual([]);
  });

  it("renders a generic filter bar on Table by default and respects filterable={false}", () => {
    const sample = (filterable?: boolean) =>
      renderToStaticMarkup(
        <Table filterable={filterable} filterPlaceholder="Filter items…">
          <TableHeader>
            <TableRow>
              <TableHead>id</TableHead>
              <TableHead>name</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableRow>
              <TableCell>row-1</TableCell>
              <TableCell>first</TableCell>
            </TableRow>
          </TableBody>
        </Table>,
      );

    const withFilter = sample();
    expect(withFilter).toContain('data-slot="table-filter"');
    expect(withFilter).toContain("Filter items…");
    expect(withFilter).toContain("row-1");

    const withoutFilter = sample(false);
    expect(withoutFilter).not.toContain('data-slot="table-filter"');
    expect(withoutFilter).toContain("row-1");
  });
});
