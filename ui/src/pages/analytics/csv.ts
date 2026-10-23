// Client-side CSV export of the table on screen.
export interface CsvModel {
  /** The file name's middle part: ops-<name>-<from>_<to>.csv */
  name: string;
  head: string[];
  rows: (string | number | null)[][];
}

const quote = (value: string | number | null): string => `"${String(value ?? "").replace(/"/g, '""')}"`;

/** buildCsv is the model as CSV text: a byte-order mark for spreadsheets, CRLF rows, every cell quoted. */
export function buildCsv(model: CsvModel): string {
  return `﻿${[model.head, ...model.rows].map((row) => row.map(quote).join(",")).join("\r\n")}`;
}

/** downloadCsv hands the model to the browser as a file download. */
export function downloadCsv(model: CsvModel, fileName: string): void {
  const url = URL.createObjectURL(new Blob([buildCsv(model)], { type: "text/csv;charset=utf-8" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = fileName;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
