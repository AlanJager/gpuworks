// The HTTP200 preflight predates ProductError codes. Translate only its exact
// known reasons; preserve unknown or more detailed diagnostics verbatim.
export function terminalPreflightReason(reason: string, translate: (key: string) => string): string {
  switch (reason) {
    case "terminal streaming is not available":
      return translate("terminalView.preflight.unavailable");
    case "web terminal is disabled in platform settings":
      return translate("terminalView.preflight.disabled");
    case "node is not connected":
      return translate("terminalView.preflight.nodeOffline");
    case "this node's agent does not support terminals; upgrade the agent":
      return translate("terminalView.preflight.agentUnsupported");
    case "too many terminal sessions on this node":
      return translate("terminalView.preflight.nodeLimit");
    case "too many terminal sessions on this platform":
      return translate("terminalView.preflight.globalLimit");
    case "this node's agent does not support container terminals":
      return translate("terminalView.preflight.containerUnsupported");
    case "runveil container shell is unavailable on this agent":
      return translate("terminalView.preflight.runveilUnavailable");
    default:
      return reason;
  }
}
