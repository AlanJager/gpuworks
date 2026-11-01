import { APIError, apiErrorText, type APIErrorTranslator } from "../../apiError";

const FILE_REASONS = new Set([
  "invalid_json_line", "missing_custom_id", "duplicate_custom_id", "invalid_method", "invalid_url", "missing_body",
  "missing_model", "streaming_not_supported", "line_too_long", "empty_file", "too_many_requests", "upload_failed",
  "mixed_endpoints",
]);
const REQUEST_REASONS = new Set(["endpoint_mismatch", "invalid_endpoint", "missing_required_parameter", "invalid_multipart"]);

export interface BatchErrorText {
  /** The error in the reader's language. */
  message: string;
  /** The server's English detail, shown beside a translated file error as the technical reason. */
  detail?: string;
}

/**
 * batchErrorText words a batch error. An invalid input file names its line
 * (line 0 is the whole file), a request error its field and reason and a file
 * whose models nothing serves those models, in the reader's language; every
 * other product error goes through the shared translation of its code.
 */
export function batchErrorText(t: APIErrorTranslator, tCommon: APIErrorTranslator, error: unknown): BatchErrorText {
  if (error instanceof APIError) {
    if (error.code === "BATCH_FILE_MODELS_UNKNOWN") {
      const params = { ...error.params, models: modelList(t, error.params) };
      return { message: apiErrorText(tCommon, new APIError(error.code, params, error.fallback, error.status)) };
    }
    const reason = String(error.params?.reason ?? "");
    if (error.code === "BATCH_FILE_INVALID" && FILE_REASONS.has(reason)) {
      const line = Number(error.params?.line ?? 0);
      const worded = t(`errors.fileReasons.${reason}`, {});
      const detail = typeof error.params?.detail === "string" ? error.params.detail : undefined;
      return { message: line > 0 ? t("errors.fileLine", { line, reason: worded }) : t("errors.fileWhole", { reason: worded }), detail };
    }
    if (error.code === "BATCH_REQUEST_INVALID" && REQUEST_REASONS.has(reason)) {
      return { message: t("errors.requestInvalid", { field: String(error.params?.field ?? ""), reason: t(`errors.requestReasons.${reason}`, {}) }) };
    }
  }
  return { message: apiErrorText(tCommon, error) };
}

/**
 * modelList is the {models, more} of a message about the models of an input
 * file as the list its text formats with the reader's list separators: the
 * names the server sent, then "N more" when it found further ones.
 */
export function modelList(t: APIErrorTranslator, params: Record<string, unknown> | undefined): string[] {
  const models = Array.isArray(params?.models) ? params.models.map(String) : [];
  const more = Number(params?.more ?? 0);
  return more > 0 ? [...models, t("errors.moreModels", { count: more })] : models;
}

/** batchWarningText words a warning of a created batch in the reader's language. */
export function batchWarningText(t: APIErrorTranslator, warning: { code: string; params?: Record<string, unknown> }): string {
  const params = warning.code === "BATCH_FILE_MODELS_SLEEP_HELD" ? { ...warning.params, models: modelList(t, warning.params) } : warning.params;
  return t(`create.warnings.${warning.code}`, { ...params, defaultValue: warning.code });
}
