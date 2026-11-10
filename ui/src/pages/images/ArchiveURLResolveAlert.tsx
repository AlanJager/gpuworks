import { Alert } from "antd";
import { useTranslation } from "react-i18next";
import { APIError, apiErrorText } from "../../apiError";

type Props = {
  error: Error;
  fallback: string;
};

/**
 * ArchiveURLResolveAlert shows one archive resolve failure.
 * Allowing a host is a checkbox on the address field, not a second action here.
 */
export default function ArchiveURLResolveAlert({ error, fallback }: Props) {
  const { t: tCommon } = useTranslation("common");
  const message = error instanceof APIError
    ? apiErrorText((key, options) => tCommon(key, { ...options, defaultValue: fallback }), error)
    : error.message;
  const blocked = error instanceof APIError && (error.code === "IMAGE_ARCHIVE_URL_POLICY_DENIED" || error.code === "IMAGE_ARCHIVE_URL_POLICY_BLOCKED");
  return (
    <Alert
      type={blocked ? "error" : "warning"}
      showIcon
      message={message}
      description={error instanceof APIError ? `HTTP ${error.status} · ${error.code}` : undefined}
    />
  );
}
