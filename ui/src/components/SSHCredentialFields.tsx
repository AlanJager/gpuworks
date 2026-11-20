import { useEffect, useId, useState } from "react";
import type { Control, FieldPath, FieldValues } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { api, type PlatformSSHKey, type SSHAuthMode } from "../api";
import { FormField } from "../ui-kit/form/FormField";
import { Notice } from "../ui-kit/Notice";
import { PasswordInput } from "../ui-kit/PasswordInput";
import { Spinner } from "../ui-kit/Spinner";
import CopyButton from "./CopyButton";
import { Field, FieldDescription, FieldLabel } from "./ui/field";
import { RadioGroup, RadioItem } from "./ui/radio-group";
import { Textarea } from "./ui/textarea";

export type { SSHAuthMode };

/** The secret fields a form holding SSH credentials carries. */
export type SSHSecretValues = { password?: string; private_key_pem?: string };

const AUTH_MODES: { value: SSHAuthMode; label: string }[] = [
  { value: "platform_key", label: "ssh.auth.platformKey" },
  { value: "password", label: "ssh.auth.password" },
  { value: "keyboard_interactive", label: "ssh.auth.keyboardInteractive" },
  { value: "private_key", label: "ssh.auth.key" },
];

const required = (message: string) => ({ validate: (value: unknown) => (typeof value === "string" && value.trim() !== "") || message });

export default function SSHCredentialFields<T extends FieldValues & SSHSecretValues>({
  control,
  authMode,
  onAuthModeChange,
}: {
  control: Control<T>;
  authMode: SSHAuthMode;
  onAuthModeChange: (mode: SSHAuthMode) => void;
}) {
  const { t } = useTranslation("nodes");
  const modeLabel = useId();
  return (
    <>
      <Field>
        <FieldLabel id={modeLabel}>{t("ssh.auth.label")}</FieldLabel>
        <RadioGroup aria-labelledby={modeLabel} value={authMode} onValueChange={(mode) => onAuthModeChange(mode as SSHAuthMode)}
          className="w-auto flex-row flex-wrap">
          {AUTH_MODES.map((mode, index) => <RadioItem key={mode.value} index={index} value={mode.value} label={t(mode.label)} />)}
        </RadioGroup>
      </Field>
      {authMode === "platform_key" && <PlatformKeyAuthorization />}
      {(authMode === "password" || authMode === "keyboard_interactive") && (
        <FormField control={control} name={"password" as FieldPath<T>} required label={t("ssh.password")} rules={required(t("ssh.passwordRequired"))}
          description={authMode === "keyboard_interactive" ? `${t("ssh.keyboardInteractiveHelp")} ${t("ssh.authorizesPlatformKey")}` : t("ssh.authorizesPlatformKey")}>
          {(binding) => <PasswordInput {...binding} value={binding.value ?? ""} autoComplete="off" />}
        </FormField>
      )}
      {authMode === "private_key" && (
        <FormField control={control} name={"private_key_pem" as FieldPath<T>} required label={t("ssh.privateKey")} rules={required(t("ssh.privateKeyRequired"))}
          description={t("ssh.authorizesPlatformKey")}>
          {(binding) => <Textarea {...binding} value={binding.value ?? ""} rows={6} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" />}
        </FormField>
      )}
    </>
  );
}

// PlatformKeyAuthorization shows the command that authorizes the server's own
// key on a target, which is all the platform_key mode asks of the operator.
export function PlatformKeyAuthorization() {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const [key, setKey] = useState<PlatformSSHKey | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    api.platformSSHKey()
      .then((k) => live && setKey(k))
      .catch((e) => live && setError((e as Error).message));
    return () => {
      live = false;
    };
  }, []);
  if (error) {
    return <Notice tone="danger" title={t("ssh.platformKey.unavailable", { detail: error })} />;
  }
  if (!key) {
    return <div><Spinner label={tCommon("picker.loading")} /></div>;
  }
  return (
    <Field>
      <FieldLabel>{t("ssh.platformKey.authorizeLabel")}</FieldLabel>
      <div className="flex items-start gap-1 rounded-lg bg-emphasis px-3 py-2">
        <code className="min-w-0 flex-1 break-all font-mono text-label">{key.authorize_command}</code>
        <CopyButton text={key.authorize_command} label={t("add.copyCommand")} size="small" />
      </div>
      <FieldDescription>{t("ssh.platformKey.authorizeHelp", { fingerprint: key.fingerprint })}</FieldDescription>
    </Field>
  );
}
