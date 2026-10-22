import { Alert } from "antd";
import { useTranslation } from "react-i18next";

// Quick-tunnel hostnames are assigned per connector run, so a node saved with
// one stops being able to reach the server as soon as the connector restarts.
// The plan asks for a hint here and is explicit that it is not a gate: the
// operator may be testing, so nothing is blocked and no field was added to the
// API or the forms for this.
export function isTemporaryAddress(raw: string): boolean {
  const value = raw.trim();
  if (value === "") {
    return false;
  }
  let host = value;
  try {
    // A bare host, host:port or full URL are all things operators paste here.
    host = new URL(value.includes("://") ? value : `https://${value}`).hostname;
  } catch {
    return false; // not an address at all; judging that is not this hint's job
  }
  // The fully-qualified spelling ends in a dot, and the lookalike suffix must
  // not match: x.trycloudflare.com.example.com is somebody else's host.
  host = host.toLowerCase().replace(/\.+$/, "");
  return host === "trycloudflare.com" || host.endsWith(".trycloudflare.com");
}

export default function TemporaryAddressHint({ value }: { value: string }) {
  const { t } = useTranslation("common");
  if (!isTemporaryAddress(value)) {
    return null;
  }
  return (
    <Alert
      type="warning"
      showIcon
      message={t("temporaryAddress.message")}
      description={t("temporaryAddress.detail")}
    />
  );
}
