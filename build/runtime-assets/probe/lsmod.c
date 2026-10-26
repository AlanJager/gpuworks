#include <fcntl.h>
#include <unistd.h>

int main(void) {
    char buffer[8192];
    ssize_t size;
    int modules = open("/proc/modules", O_RDONLY);
    if (modules < 0) return 1;
    while ((size = read(modules, buffer, sizeof(buffer))) > 0) {
        ssize_t written = 0;
        while (written < size) {
            ssize_t next = write(STDOUT_FILENO, buffer + written, (size_t)(size - written));
            if (next <= 0) return 2;
            written += next;
        }
    }
    return size < 0;
}
