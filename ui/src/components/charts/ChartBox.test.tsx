import type { ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { Columns } from "./Columns";
import { Heatmap } from "./Heatmap";
import { LineChart, type LineSeries } from "./LineChart";
import { SmallMultiples } from "./SmallMultiples";

// Rendered on the server a chart never measures its width: the moment a chart
// used to take no space, so the page moved when it drew. The only inline
// height in a chart's markup is its ChartBox.
const boxHeights = (chart: ReactElement) => [...renderToStaticMarkup(chart).matchAll(/style="height:(\d+)px"/g)].map((m) => Number(m[1]));

const line = (id: string, values: (number | null)[]): LineSeries => ({
  id,
  label: id,
  color: "var(--series-1)",
  points: values.map((v, i) => ({ t: i * 5_000, v })),
});
const hours = (vs: (number | null)[]) => vs.map((v, i) => ({ key: String(i), label: String(i), v }));

describe("a chart takes its height before it measures, whatever its data", () => {
  it.each<[string, ReactElement, ReactElement, number]>([
    [
      "LineChart",
      <LineChart series={[]} height={150} format={String} />,
      <LineChart series={[line("a", [1, 5, null, 2]), line("b", [9, 0, 3, 3])]} height={150} format={String} />,
      150,
    ],
    [
      "Columns",
      <Columns columns={[]} series={[{ id: "n", label: "n", color: "var(--series-1)" }]} height={90} format={String} />,
      <Columns
        columns={[3, 0, 12].map((v, i) => ({ key: String(i), label: String(i), values: [v, v / 2] }))}
        series={[
          { id: "a", label: "a", color: "var(--series-1)" },
          { id: "b", label: "b", color: "var(--series-2)" },
        ]}
        height={90}
        format={String}
      />,
      90,
    ],
    [
      "Heatmap",
      <Heatmap rows={[{ id: "a", label: "a", cells: [] }, { id: "b", label: "b", cells: [] }]} format={String} />,
      <Heatmap
        rows={[
          { id: "a", label: "a", cells: hours([0.1, 0.9, null]) },
          { id: "b", label: "b", cells: hours([1, 0, 0.5]) },
        ]}
        format={String}
      />,
      2 * 22 + 20,
    ],
    [
      "SmallMultiples",
      <SmallMultiples series={[line("a", []), line("b", [])]} yMax={1} format={String} />,
      <SmallMultiples series={[line("a", [0.2, 0.4, 0.9]), line("b", [0.5, null, 0.1])]} yMax={1} format={String} />,
      2 * 40 + 16,
    ],
  ])("%s", (_name, empty, full, height) => {
    expect(boxHeights(empty)).toEqual([height]);
    expect(boxHeights(full)).toEqual([height]);
  });
});
