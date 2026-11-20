import type { StorageFormValues } from "./storageForm";

/** A storage another page offers to add, as the server suggested it (dto.SlurmSharedStorageHint). */
export interface StorageAddSuggestion {
  name: string;
  type: string;
  path: string;
  label_key: string;
  label_value: string;
}

/** The router state that opens the storage page's add form with a suggestion. */
export function storageAddState(suggestion: StorageAddSuggestion) {
  return { addStorage: suggestion };
}

/**
 * The form values a suggestion in the router state starts the add form
 * with: its name, type and directory, for the nodes carrying its label.
 */
export function storagePresetFromState(state: unknown): Partial<StorageFormValues> | undefined {
  const s = (state as { addStorage?: StorageAddSuggestion } | null)?.addStorage;
  if (!s || !s.type || !s.path) return undefined;
  return {
    name: s.name, type: s.type as StorageFormValues["type"], path: s.path,
    scope: "labels", allNodes: false, labels: [{ key: s.label_key, value: s.label_value }],
  };
}
