// cpuClock starts a stopwatch of the CPU time this test process spends from
// now on, in milliseconds. Speed budgets are asserted on it rather than on
// the wall clock: CI runs a test file per core, and on a loaded runner the
// wall time of the same work stretches several fold (an 8000-card render
// measured 4 s to 22 s there) while its CPU time stays put. The wall time
// is still worth logging; it just says more about the host than the code.
export function cpuClock(): () => number {
  const start = process.cpuUsage();
  return () => {
    const used = process.cpuUsage(start);
    return (used.user + used.system) / 1000;
  };
}
