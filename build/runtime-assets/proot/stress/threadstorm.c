/*
 * threadstorm: reproduce the many-thread starvation of a PRoot tracer.
 *
 * A launcher forks N "ranks".  Each rank has a main thread that
 *   1. starts a watchdog (ends the rank at a hard deadline),
 *   2. starts SLEEPERS threads that only sleep (nanosleep(2) is not traced
 *      under seccomp, so they never stop),
 *   3. starts BUSY threads that loop on stat(2) (a syscall PRoot traces, so
 *      every call is a ptrace stop the tracer has to serve) plus a spawner
 *      that keeps creating short-lived busy threads, which are always newer
 *      than every other thread of the rank (the kernel reports the newest
 *      tracee first),
 *   4. loops on stat(2) itself for RUN seconds and counts how often it got
 *      through.
 * Under a fair tracer every rank's main thread keeps making progress.  Under a
 * starving one the main thread sits in a tracing-stop: thread start-up crawls
 * (step 2 and 3) and step 4 makes (almost) no progress.
 *
 * Output, one line per rank on stdout (run-stress.sh summarises them):
 *   rank=<i> setup_ms=<ms> run_ms=<ms> main_iters=<n> max_gap_ms=<ms> capped=<0|1>
 */
#define _GNU_SOURCE
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static volatile int stop;
static volatile int capped;

static double now_ms(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec * 1e3 + ts.tv_nsec / 1e6;
}

static void *sleeper(void *arg)
{
	(void)arg;
	while (!stop)
		usleep(20000);
	return NULL;
}

static void *busy(void *arg)
{
	struct stat st;

	(void)arg;
	while (!stop)
		stat("/", &st);
	return NULL;
}

static void *short_lived(void *arg)
{
	struct stat st;
	int i;

	(void)arg;
	for (i = 0; i < 200 && !stop; i++)
		stat("/", &st);
	return NULL;
}

static void *spawner(void *arg)
{
	(void)arg;
	while (!stop) {
		pthread_t t[8];
		int i, started = 0;

		for (i = 0; i < 8 && !stop; i++)
			if (pthread_create(&t[started], NULL, short_lived, NULL) == 0)
				started++;
		for (i = 0; i < started; i++)
			pthread_join(t[i], NULL);
	}
	return NULL;
}

/* Ends the rank without issuing any traced syscall: the main thread may be the
 * one that is starved, so it can't be the one to flip the flag in that case.  */
static void *watchdog(void *arg)
{
	double deadline = *(double *)arg;

	while (!stop && now_ms() < deadline)
		usleep(5000);
	if (!stop)
		capped = 1;
	stop = 1;
	return NULL;
}

static int run_rank(int rank, int sleepers, int busies, double run_s, double cap_s)
{
	pthread_t tid;
	struct stat st;
	double start = now_ms(), setup_end, last, gap, max_gap = 0, run_end;
	double deadline = start + cap_s * 1e3;
	unsigned long iters = 0;
	int i;

	pthread_create(&tid, NULL, watchdog, &deadline);
	for (i = 0; i < sleepers && !stop; i++)
		pthread_create(&tid, NULL, sleeper, NULL);
	for (i = 0; i < busies && !stop; i++)
		pthread_create(&tid, NULL, busy, NULL);
	if (!stop)
		pthread_create(&tid, NULL, spawner, NULL);

	setup_end = last = now_ms();
	run_end = setup_end + run_s * 1e3;
	while (!stop && last < run_end) {
		stat("/proc/self", &st);
		iters++;
		gap = now_ms() - last;
		last = now_ms();
		if (gap > max_gap)
			max_gap = gap;
	}
	/* The final stall counts too: how long was the main thread stuck before
	 * it noticed the end of the run?  */
	gap = now_ms() - last;
	if (gap > max_gap)
		max_gap = gap;
	printf("rank=%d setup_ms=%.0f run_ms=%.0f main_iters=%lu max_gap_ms=%.0f capped=%d\n",
	       rank, setup_end - start, last - setup_end, iters, max_gap, capped);
	fflush(stdout);
	stop = 1;
	return 0;
}

int main(int argc, char **argv)
{
	int ranks = argc > 1 ? atoi(argv[1]) : 8;
	int threads = argc > 2 ? atoi(argv[2]) : 300;
	int busies = argc > 3 ? atoi(argv[3]) : 60;
	double run_s = argc > 4 ? atof(argv[4]) : 15;
	double cap_s = argc > 5 ? atof(argv[5]) : 120;
	int sleepers = threads - busies - 3; /* main, watchdog, spawner */
	int i;

	if (sleepers < 0)
		sleepers = 0;
	for (i = 0; i < ranks; i++) {
		pid_t pid = fork();

		if (pid == 0)
			_exit(run_rank(i, sleepers, busies, run_s, cap_s));
		if (pid < 0) {
			perror("fork");
			return 2;
		}
	}
	for (i = 0; i < ranks; i++) {
		int status;

		wait(&status);
	}
	return 0;
}
