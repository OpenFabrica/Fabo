#import <Foundation/Foundation.h>
#import <Virtualization/Virtualization.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <unistd.h>
#include "virtualization.h"
#include "_cgo_export.h"

@interface FaboRelay : NSObject {
@public
    dispatch_queue_t queue;
    VZVirtioSocketConnection *connection;
}
@end

@implementation FaboRelay
@end

@interface FaboVM : NSObject <VZVirtualMachineDelegate, VZVirtioSocketListenerDelegate> {
@public
    dispatch_queue_t queue;
    VZVirtualMachine *vm;
    VZVirtioSocketListener *listener;
    VZVirtioSocketListener *hostListener;
    VZVirtioSocketConnection *control;
    int notify;
    uint32_t agentPort;
    uint32_t hostPort;
    uintptr_t hostHandle;
}
@end

@implementation FaboVM
- (void)dealloc {
    close(notify);
}

- (BOOL)listener:(VZVirtioSocketListener *)source
    shouldAcceptNewConnection:(VZVirtioSocketConnection *)connection
    fromSocketDevice:(VZVirtioSocketDevice *)device {
    if (source == hostListener) {
        int fd = fcntl(connection.fileDescriptor, F_DUPFD_CLOEXEC, 0);
        if (fd < 0) {
            return NO;
        }
        FaboRelay *relay = [FaboRelay new];
        relay->queue = queue;
        relay->connection = connection;
        void *owner = (__bridge_retained void *)relay;
        if (!faboAcceptHost(hostHandle, fd, owner)) {
            close(fd);
            (void)CFBridgingRelease(owner);
            return NO;
        }
        return YES;
    }
    if (control != nil) {
        return NO;
    }
    // The framework closes the stream when this object is released,
    // even if the daemon has a duplicate file descriptor.
    control = connection;
    char event = 'C';
    if (write(notify, &event, 1) != 1) {
        control = nil;
        return NO;
    }
    return YES;
}

- (void)guestDidStopVirtualMachine:(VZVirtualMachine *)machine {
    char event = 'S';
    (void)write(notify, &event, 1);
}

- (void)virtualMachine:(VZVirtualMachine *)machine didStopWithError:(NSError *)error {
    char event = 'E';
    (void)write(notify, &event, 1);
}
@end

static NSURL *fileURL(const char *path) {
    return [NSURL fileURLWithFileSystemRepresentation:path isDirectory:NO relativeToURL:nil];
}

static VZVirtualMachineConfiguration *configuration(
    const char *kernel, const char *initrd, const char *disk, const char *console, const char *mac,
    char *const *shares, size_t shareCount, const char *shareTag, uint32_t cpus, uint64_t memory, NSError **error) {
    VZLinuxBootLoader *loader = [[VZLinuxBootLoader alloc] initWithKernelURL:fileURL(kernel)];
    loader.initialRamdiskURL = fileURL(initrd);
    loader.commandLine = @"console=hvc0 rdinit=/init panic=-1 rcupdate.rcu_expedited=1 swiotlb=noforce";
    VZVirtualMachineConfiguration *config = [VZVirtualMachineConfiguration new];
    config.bootLoader = loader;
    config.CPUCount = cpus;
    config.memorySize = memory;
    config.socketDevices = @[[[VZVirtioSocketDeviceConfiguration alloc] init]];
    config.entropyDevices = @[[[VZVirtioEntropyDeviceConfiguration alloc] init]];
    if (*mac != '\0') {
        VZVirtioNetworkDeviceConfiguration *network =
            [[VZVirtioNetworkDeviceConfiguration alloc] init];
        network.MACAddress = [[VZMACAddress alloc] initWithString:[NSString stringWithUTF8String:mac]];
        network.attachment = [[VZNATNetworkDeviceAttachment alloc] init];
        config.networkDevices = @[network];
    }
    NSMutableArray<VZDirectorySharingDeviceConfiguration *> *shareDevices = [NSMutableArray new];
    if (shareCount > 0) {
        NSMutableDictionary<NSString *, VZSharedDirectory *> *directories = [NSMutableDictionary new];
        for (size_t i = 0; i < shareCount; i++) {
            NSURL *url = [NSURL fileURLWithFileSystemRepresentation:shares[i] isDirectory:YES relativeToURL:nil];
            directories[[@(i) stringValue]] = [[VZSharedDirectory alloc] initWithURL:url readOnly:NO];
        }
        VZVirtioFileSystemDeviceConfiguration *fs = [[VZVirtioFileSystemDeviceConfiguration alloc]
            initWithTag:[NSString stringWithUTF8String:shareTag]];
        if (shareCount == 1) {
            fs.share = [[VZSingleDirectoryShare alloc] initWithDirectory:directories[@"0"]];
        } else {
            fs.share = [[VZMultipleDirectoryShare alloc] initWithDirectories:directories];
        }
        [shareDevices addObject:fs];
        loader.commandLine = [loader.commandLine stringByAppendingString:@" fabo.share=1"];
    }
#if defined(__arm64__)
    if (VZLinuxRosettaDirectoryShare.availability == VZLinuxRosettaAvailabilityInstalled) {
        NSError *rosettaError = nil;
        VZLinuxRosettaDirectoryShare *share = [[VZLinuxRosettaDirectoryShare alloc] initWithError:&rosettaError];
        if (share != nil) {
            VZVirtioFileSystemDeviceConfiguration *fs = [[VZVirtioFileSystemDeviceConfiguration alloc] initWithTag:@"rosetta"];
            fs.share = share;
            [shareDevices addObject:fs];
            loader.commandLine = [loader.commandLine stringByAppendingString:@" fabo.rosetta=1"];
        } else {
            NSLog(@"Fabo Rosetta share: %@", rosettaError.localizedDescription);
        }
    }
#endif
    config.directorySharingDevices = shareDevices;
    if (*disk != '\0') {
        VZDiskImageStorageDeviceAttachment *attachment =
            [[VZDiskImageStorageDeviceAttachment alloc] initWithURL:fileURL(disk) readOnly:NO error:error];
        if (attachment == nil) {
            return nil;
        }
        config.storageDevices = @[[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:attachment]];
    }
    // The guest boots with console=hvc0 even when host logging is disabled.
    VZSerialPortAttachment *attachment = *console != '\0'
        ? [[VZFileSerialPortAttachment alloc] initWithURL:fileURL(console) append:YES error:error]
        : [[VZFileHandleSerialPortAttachment alloc] initWithFileHandleForReading:nil
                                                          fileHandleForWriting:nil];
    if (attachment == nil) {
        return nil;
    }
    VZVirtioConsoleDeviceSerialPortConfiguration *serial =
        [[VZVirtioConsoleDeviceSerialPortConfiguration alloc] init];
    serial.attachment = attachment;
    config.serialPorts = @[serial];
    return [config validateWithError:error] ? config : nil;
}

void *fabo_vm_start(const char *kernel, const char *initrd, const char *disk,
                    const char *console, const char *mac, char *const *shares, size_t shareCount,
                    const char *shareTag, uint32_t cpus, uint64_t memory,
                    uint32_t port, uint32_t hostPort, uintptr_t hostHandle, int notify, char **error) {
    @autoreleasepool {
        FaboVM *host = [FaboVM new];
        host->notify = fcntl(notify, F_DUPFD_CLOEXEC, 0);
        if (host->notify < 0) {
            *error = strdup("duplicate VM notification pipe failed");
            return NULL;
        }
        NSError *failure = nil;
        VZVirtualMachineConfiguration *config = configuration(kernel, initrd, disk, console, mac, shares, shareCount, shareTag, cpus, memory, &failure);
        if (config == nil) {
            *error = strdup(failure.localizedDescription.UTF8String ?: "invalid VM configuration");
            return NULL;
        }
        host->agentPort = port;
        host->hostPort = hostPort;
        host->hostHandle = hostHandle;
        host->queue = dispatch_queue_create("dev.fabo.go-vm", DISPATCH_QUEUE_SERIAL);
        dispatch_semaphore_t ready = dispatch_semaphore_create(0);
        __block NSError *startError = nil;
        dispatch_sync(host->queue, ^{
            host->vm = [[VZVirtualMachine alloc] initWithConfiguration:config queue:host->queue];
            host->vm.delegate = host;
            host->listener = [VZVirtioSocketListener new];
            host->listener.delegate = host;
            host->hostListener = [VZVirtioSocketListener new];
            host->hostListener.delegate = host;
            VZVirtioSocketDevice *device = (VZVirtioSocketDevice *)host->vm.socketDevices.firstObject;
            [device setSocketListener:host->listener forPort:port];
            [device setSocketListener:host->hostListener forPort:hostPort];
            [host->vm startWithCompletionHandler:^(NSError *err) {
                startError = err;
                dispatch_semaphore_signal(ready);
            }];
        });
        dispatch_semaphore_wait(ready, DISPATCH_TIME_FOREVER);
        if (startError != nil) {
            *error = strdup(startError.localizedDescription.UTF8String ?: "VM startup failed");
            fabo_vm_release((__bridge_retained void *)host);
            return NULL;
        }
        return (__bridge_retained void *)host;
    }
}

int fabo_vm_control(void *machine) {
    FaboVM *host = (__bridge FaboVM *)machine;
    __block int fd = -1;
    dispatch_sync(host->queue, ^{
        if (host->control != nil) {
            fd = fcntl(host->control.fileDescriptor, F_DUPFD_CLOEXEC, 0);
        }
    });
    return fd;
}

void fabo_vm_disconnect(void *machine) {
    FaboVM *host = (__bridge FaboVM *)machine;
    dispatch_sync(host->queue, ^{
        if (host->control != nil) {
            shutdown(host->control.fileDescriptor, SHUT_RDWR);
        }
    });
}

int fabo_vm_stop(void *machine, char **error) {
    @autoreleasepool {
        FaboVM *host = (__bridge FaboVM *)machine;
        dispatch_semaphore_t done = dispatch_semaphore_create(0);
        __block BOOL stopping = NO;
        __block NSError *stopError = nil;
        dispatch_sync(host->queue, ^{
            if (host->vm.canStop) {
                stopping = YES;
                [host->vm stopWithCompletionHandler:^(NSError *err) {
                    stopError = err;
                    dispatch_semaphore_signal(done);
                }];
            }
        });
        if (stopping) {
            dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
        }
        if (stopError != nil) {
            *error = strdup(stopError.localizedDescription.UTF8String ?: "VM stop failed");
            return -1;
        }
        return 0;
    }
}

void fabo_vm_release(void *machine) {
    FaboVM *host = (__bridge_transfer FaboVM *)machine;
    dispatch_sync(host->queue, ^{
        [(VZVirtioSocketDevice *)host->vm.socketDevices.firstObject removeSocketListenerForPort:host->agentPort];
        [(VZVirtioSocketDevice *)host->vm.socketDevices.firstObject removeSocketListenerForPort:host->hostPort];
        host->vm.delegate = nil;
        host->listener = nil;
        host->hostListener = nil;
        host->control = nil;
        host->vm = nil;
    });
}

void *fabo_vm_connect(void *machine, uint32_t port, int *fd, char **error) {
    @autoreleasepool {
        FaboVM *host = (__bridge FaboVM *)machine;
        dispatch_semaphore_t ready = dispatch_semaphore_create(0);
        __block FaboRelay *relay = nil;
        __block NSError *failure = nil;
        dispatch_sync(host->queue, ^{
            VZVirtioSocketDevice *device = (VZVirtioSocketDevice *)host->vm.socketDevices.firstObject;
            [device connectToPort:port completionHandler:^(VZVirtioSocketConnection *connection, NSError *err) {
                if (connection != nil) {
                    relay = [FaboRelay new];
                    relay->queue = host->queue;
                    relay->connection = connection;
                }
                failure = err;
                dispatch_semaphore_signal(ready);
            }];
        });
        if (dispatch_semaphore_wait(ready, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC)) != 0) {
            *error = strdup("guest vsock connection timed out");
            return NULL;
        }
        if (relay == nil) {
            *error = strdup(failure.localizedDescription.UTF8String ?: "guest vsock connection failed");
            return NULL;
        }
        __block int duplicate = -1;
        dispatch_sync(relay->queue, ^{
            duplicate = fcntl(relay->connection.fileDescriptor, F_DUPFD_CLOEXEC, 0);
        });
        if (duplicate < 0) {
            *error = strdup("duplicate VM relay socket failed");
            fabo_vm_close_relay((__bridge_retained void *)relay);
            return NULL;
        }
        *fd = duplicate;
        return (__bridge_retained void *)relay;
    }
}

void fabo_vm_close_relay(void *opaque) {
    FaboRelay *relay = (__bridge_transfer FaboRelay *)opaque;
    dispatch_sync(relay->queue, ^{
        shutdown(relay->connection.fileDescriptor, SHUT_RDWR);
        relay->connection = nil;
    });
}

void fabo_vm_limits(uint32_t *minCPU, uint32_t *maxCPU, uint64_t *minMemory, uint64_t *maxMemory) {
    *minCPU = (uint32_t)VZVirtualMachineConfiguration.minimumAllowedCPUCount;
    *maxCPU = (uint32_t)VZVirtualMachineConfiguration.maximumAllowedCPUCount;
    *minMemory = VZVirtualMachineConfiguration.minimumAllowedMemorySize;
    *maxMemory = VZVirtualMachineConfiguration.maximumAllowedMemorySize;
}
