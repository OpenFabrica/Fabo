#include <stdint.h>
#include <stddef.h>

void *fabo_vm_start(const char *kernel, const char *initrd, const char *disk,
                    const char *console, const char *mac, int networkFD, char *const *shares, size_t shareCount,
                    const char *shareTag, uint32_t cpus, uint64_t memory,
                    uint32_t port, uint32_t hostPort, uintptr_t hostHandle, int notify, char **error);
int fabo_vm_control(void *machine);
void fabo_vm_disconnect(void *machine);
int fabo_vm_stop(void *machine, char **error);
void fabo_vm_release(void *machine);
int fabo_vm_set_memory_target(void *machine, uint64_t bytes);
void *fabo_vm_connect(void *machine, uint32_t port, int *fd, char **error);
void fabo_vm_close_relay(void *relay);
void fabo_vm_limits(uint32_t *minCPU, uint32_t *maxCPU, uint64_t *minMemory, uint64_t *maxMemory);
