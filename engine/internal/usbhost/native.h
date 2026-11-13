#include <stdint.h>
#include <stddef.h>
char *kiri_usb_list(char **error);
void *kiri_usb_capture(uint32_t location, int fd, char **error);
int32_t kiri_usb_control(void *, uint8_t, uint8_t, uint16_t, uint16_t, uint16_t, void *, size_t, size_t *);
int32_t kiri_usb_transfer(void *, uint8_t, void *, size_t, size_t *);
int32_t kiri_usb_configure(void *, uint8_t);
int32_t kiri_usb_alternate(void *, uint8_t, uint8_t);
int32_t kiri_usb_clear(void *, uint8_t);
void kiri_usb_abort(void *, uint8_t);
void kiri_usb_close(void *);
void kiri_usb_dispose(void *);
