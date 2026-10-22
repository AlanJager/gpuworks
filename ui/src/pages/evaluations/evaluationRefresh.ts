export type RefreshCoalescer = { trigger: () => void; dispose: () => void };

// Coalesces bursty SSE notifications, prevents overlapping requests, and runs
// one trailing refresh when changes arrive while a refresh is in flight.
export function createRefreshCoalescer(refresh: () => Promise<void>, intervalMs = 1000): RefreshCoalescer {
	let disposed = false;
	let timer: ReturnType<typeof setTimeout> | undefined;
	let inFlight = false;
	let trailing = false;
	let lastStart = 0;
	const run = async () => {
		timer = undefined;
		if (disposed) return;
		if (inFlight) { trailing = true; return; }
		inFlight = true;
		lastStart = Date.now();
		try { await refresh(); } finally {
			inFlight = false;
			if (trailing && !disposed) { trailing = false; trigger(); }
		}
	};
	const trigger = () => {
		if (disposed) return;
		if (inFlight) { trailing = true; return; }
		if (timer) return;
		const delay = Math.max(0, intervalMs - (Date.now() - lastStart));
		timer = setTimeout(() => { void run(); }, delay);
	};
	return { trigger, dispose: () => { disposed = true; if (timer) clearTimeout(timer); } };
}
