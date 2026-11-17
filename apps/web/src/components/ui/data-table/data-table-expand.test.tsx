import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "./data-table";

interface Row {
  id: string;
  name: string;
  open: boolean;
}
const columns: ColumnDef<Row>[] = [
  { id: "name", header: "Name", accessorKey: "name" },
];
const data: Row[] = [
  { id: "a", name: "alpha", open: true },
  { id: "b", name: "beta", open: false },
  { id: "c", name: "gamma", open: true },
];

describe("DataTable expansion", () => {
  // Four callers (networks, system backups, certificates, audit) pass renderDetailPanel
  // and expect every row to open: that default is load-bearing.
  it("expands every row by default", () => {
    render(
      <DataTable
        columns={columns}
        data={data}
        getRowId={(r) => r.id}
        renderDetailPanel={({ row }) => (
          <div>detail of {row.original.name}</div>
        )}
      />,
    );
    const toggles = screen.getAllByRole("button", { name: "Expand row" });
    expect(toggles).toHaveLength(3);
    fireEvent.click(toggles[1]);
    expect(screen.getByText("detail of beta")).toBeTruthy();
  });

  it("renders no chevron for a row that cannot expand, and the others still open", () => {
    render(
      <DataTable
        columns={columns}
        data={data}
        getRowId={(r) => r.id}
        getRowCanExpand={(row) => row.original.open}
        renderDetailPanel={({ row }) => (
          <div>detail of {row.original.name}</div>
        )}
      />,
    );
    const toggles = screen.getAllByRole("button", { name: "Expand row" });
    expect(toggles).toHaveLength(2);
    fireEvent.click(toggles[0]);
    expect(screen.getByText("detail of alpha")).toBeTruthy();
    expect(screen.queryByText("detail of beta")).toBeNull();
  });
});
