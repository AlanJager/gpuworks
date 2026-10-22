import { shellQuote } from "./shellQuote";

// A projection of the resolved launch, shared by preview, copy and instance
// details. No parameter defaults or launch policy belong in this renderer.
export function launchCommand(launch: {
  env?: Record<string, string>;
  pre_start_script?: string;
  pre_start_argv?: string[];
  command?: string[];
  entrypoint?: string[];
  argv?: string[];
}) {
  const command = (
    launch.command || [...(launch.entrypoint || []), ...(launch.argv || [])]
  )
    .map(shellQuote)
    .join(" ");
  const environment = Object.entries(launch.env || {})
    .map(([key, value]) => `export ${key}=${shellQuote(value)}`)
    .join("\n");
  const preparation =
    launch.pre_start_script ||
    (launch.pre_start_argv || []).map(shellQuote).join(" ");
  const script = [
    "set -eu",
    environment,
    preparation,
    command ? `exec ${command}` : "",
  ]
    .filter(Boolean)
    .join("\n");
  return { command, environment, preparation, script };
}
