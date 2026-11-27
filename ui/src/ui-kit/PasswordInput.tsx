import { forwardRef, useState, type ComponentPropsWithoutRef } from "react";
import { useTranslation } from "react-i18next";
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput } from "../components/ui/input-group";
import { ConsoleIcon } from "./consoleIcons";

type PasswordInputProps = Omit<ComponentPropsWithoutRef<typeof InputGroupInput>, "type">;

/** A password field with a show/hide toggle, composed from Zeron's input group. */
export const PasswordInput = forwardRef<HTMLInputElement, PasswordInputProps>((props, ref) => {
  const { t } = useTranslation();
  const [shown, setShown] = useState(false);
  const Icon = shown ? ConsoleIcon.hide : ConsoleIcon.show;
  return (
    <InputGroup>
      {/* A revealed secret is text to the browser: it must not be spellchecked
          (enhanced spellcheck sends it to a cloud service) or corrected. */}
      <InputGroupInput ref={ref} type={shown ? "text" : "password"} spellCheck={false} autoCapitalize="off" autoCorrect="off" {...props} />
      <InputGroupAddon align="inline-end">
        <InputGroupButton iconOnly aria-label={t(shown ? "actions.hidePassword" : "actions.showPassword")} aria-pressed={shown}
          onClick={() => setShown(!shown)} disabled={props.disabled}>
          <Icon />
        </InputGroupButton>
      </InputGroupAddon>
    </InputGroup>
  );
});
PasswordInput.displayName = "PasswordInput";
