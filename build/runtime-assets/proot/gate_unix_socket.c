#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

int main(int argc, char **argv) {
	if (argc != 3) {
		fprintf(stderr, "usage: %s SOCKET READY\n", argv[0]);
		return 2;
	}
	int fd = socket(AF_UNIX, SOCK_STREAM, 0);
	if (fd < 0) {
		perror("socket");
		return 1;
	}
	struct sockaddr_un addr = {0};
	addr.sun_family = AF_UNIX;
	if (strlen(argv[1]) >= sizeof(addr.sun_path)) {
		fprintf(stderr, "socket path is too long\n");
		return 2;
	}
	strcpy(addr.sun_path, argv[1]);
	if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
		fprintf(stderr, "bind %s: %s\n", argv[1], strerror(errno));
		return 1;
	}
	FILE *ready = fopen(argv[2], "w");
	if (ready == NULL) {
		fprintf(stderr, "open ready file %s: %s\n", argv[2], strerror(errno));
		return 1;
	}
	fputs("ready\n", ready);
	fclose(ready);
	sleep(10);
	return 0;
}
