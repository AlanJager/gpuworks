// rc-overflow schedules through MessageChannel and leaves its ports open.
// Close only channels created by this test process after all assertions.
export function trackMessageChannels() {
  const channels: MessageChannel[] = [];
  const OriginalChannel = globalThis.MessageChannel;
  class TrackedChannel extends OriginalChannel {
    constructor() { super(); channels.push(this); }
  }
  Object.defineProperty(globalThis, "MessageChannel", { value: TrackedChannel, configurable: true });
  return () => {
    for (const channel of channels) { channel.port1.close(); channel.port2.close(); }
    // Defined, not assigned: helpers/dom.ts installs MessageChannel read-only.
    Object.defineProperty(globalThis, "MessageChannel", { value: OriginalChannel, configurable: true });
  };
}
