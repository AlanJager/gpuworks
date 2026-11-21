// WakeRefusal is one group of sleepers a wake could not put back: the cards
// that cannot take them, how far short they are and who holds the memory.
// held_outside says a process outside the platform holds a card, which no
// sleep order of ours can free.
export interface WakeRefusal {
  instances: string[];
  node?: string;
  gpu_indexes?: number[];
  occupants?: string[];
  shortfall_gib?: string;
  held_outside?: boolean;
}

// WakeResult is a wake order's answer; a partial wake lists what it woke and
// the sleepers whose cards could not take them back. A wake that waits for
// room (wait or yield) answers 202 with status "waiting", what it woke at
// once, what waits (waiting) and the deployments put to sleep to make room
// (yielded); 200 "waking" when everything fit.
export interface WakeResult {
  status: string;
  woken?: string[];
  refused?: WakeRefusal[];
  waiting?: string[];
  yielded?: string[];
  // yielded_held are the yielded occupants that never yield by policy: a
  // request does not wake them. A request wakes the other yielded ones.
  yielded_held?: string[];
}

// WakeOptions are the ways a wake order may proceed: role limits it to one
// PD role, partial wakes what fits, wait leaves the rest waiting for room,
// yield first puts the occupants of the needed cards to sleep (at yieldLevel,
// 1 unless the server must fall back to 2).
export interface WakeOptions {
  role?: string;
  partial?: boolean;
  wait?: boolean;
  yield?: boolean;
  yieldLevel?: 1 | 2;
}
