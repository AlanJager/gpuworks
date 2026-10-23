import { createContext, useContext, useEffect } from "react";
import type { CsvModel } from "./csv";

// The toolbar's export button exports whatever table the active tab is
// showing: the tab registers its rows here and withdraws them when it goes.
export const ExportContext = createContext<(model: CsvModel | null) => void>(() => {});

export function useExportModel(model: CsvModel | null): void {
  const set = useContext(ExportContext);
  useEffect(() => {
    set(model);
    return () => set(null);
  }, [model, set]);
}
