//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <IOKit/IOKitLib.h>
#import <IOKit/IOMessage.h>
#import <IOUSBHost/IOUSBHost.h>
#include <sys/socket.h>
#include <unistd.h>
#include "native.h"

static id property(io_service_t service, NSString *key) {
    return CFBridgingRelease(IORegistryEntryCreateCFProperty(service, (__bridge CFStringRef)key, NULL, 0));
}
static uint32_t number(io_service_t service, NSString *key) {
    id n = property(service, key); return [n isKindOfClass:[NSNumber class]] ? [n unsignedIntValue] : 0;
}
static NSString *string(io_service_t service, NSString *key) {
    id s = property(service, key); return [s isKindOfClass:[NSString class]] ? s : @"";
}
static int32_t failure(NSError *error) {
    switch ((uint32_t)error.code) {
        case 0xe0005000: case 0xe000404f: return -32;
        case 0xe00002eb: return -104;
        case 0xe00002d6: return -110;
        case 0xe00002e8: return -75;
        case 0xe00002c0: case 0xe00002ed: case 0xe00002d8: case 0xe00002c7: return -19;
        default: return -71;
    }
}
static void describe(char **out, NSString *message) { if (out) *out = strdup(message.UTF8String ?: "USB error"); }
char *fabo_usb_list(char **error) {
    @autoreleasepool {
        io_iterator_t iterator = 0;
        if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOUSBHostDevice"), &iterator) != KERN_SUCCESS) {
            describe(error, @"the I/O Registry did not answer"); return NULL;
        }
        NSMutableArray *devices = [NSMutableArray array]; io_service_t service;
        while ((service = IOIteratorNext(iterator))) {
            if (number(service, @"bDeviceClass") != 9 && property(service, @"locationID")) {
                NSString *name = string(service, @"USB Product Name");
                if (!name.length) name = string(service, @"kUSBProductString");
                [devices addObject:@{@"Location": [NSString stringWithFormat:@"0x%08x", number(service,@"locationID")],
                    @"Name": name.length ? name : @"USB device", @"Vendor": @(number(service,@"idVendor")),
                    @"Product": @(number(service,@"idProduct")), @"Speed": @(number(service,@"Device Speed")),
                    @"Address": @(number(service,@"USB Address")), @"Kind": @"usb"}];
            }
            IOObjectRelease(service);
        }
        IOObjectRelease(iterator);
        NSData *data = [NSJSONSerialization dataWithJSONObject:devices options:0 error:NULL];
        return strdup([[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding].UTF8String);
    }
}

@interface FaboUSBDevice : NSObject
@property(nonatomic,strong) IOUSBHostDevice *device;
@property(nonatomic) io_service_t service;
@property(nonatomic,strong) NSMutableDictionary<NSNumber *,IOUSBHostInterface *> *interfaces;
@property(nonatomic,strong) NSMutableDictionary<NSNumber *,IOUSBHostPipe *> *pipes;
@property(nonatomic,strong) NSMutableDictionary<NSNumber *,NSNumber *> *owners;
@property(atomic) BOOL gone;
@property(atomic) BOOL closed;
@property(atomic) int fd;
- (void)openInterfaces;
- (void)closeInterfaces;
- (IOUSBHostPipe *)pipe:(uint8_t)address;
@end
@implementation FaboUSBDevice
- (void)openInterfaces {
    @synchronized(self) {
        for (int attempt=0;attempt<40 && !self.gone && !self.closed;attempt++) {
            io_iterator_t it=0;
            if (IORegistryEntryGetChildIterator(self.service,kIOServicePlane,&it)==KERN_SUCCESS) {
                io_service_t child;
                while ((child=IOIteratorNext(it))) {
                    if (IOObjectConformsTo(child,"IOUSBHostInterface")) {
                        NSNumber *n=@(number(child,@"bInterfaceNumber"));
                        if (!self.interfaces[n]) {
                            NSError *error=nil;
                            IOUSBHostInterface *interface=[[IOUSBHostInterface alloc] initWithIOService:child options:0 queue:nil error:&error interestHandler:nil];
                            if (interface) self.interfaces[n]=interface;
                        }
                    }
                    IOObjectRelease(child);
                }
                IOObjectRelease(it);
            }
            if (self.interfaces.count) return;
            usleep(50000);
        }
    }
}
- (void)closeInterfaces {
    @synchronized(self) {
        [self.pipes removeAllObjects]; [self.owners removeAllObjects];
        for (IOUSBHostInterface *interface in self.interfaces.allValues) [interface destroy];
        [self.interfaces removeAllObjects];
    }
}
- (IOUSBHostPipe *)pipe:(uint8_t)address {
    @synchronized(self) {
        if (self.closed || self.gone) return nil;
        if (self.pipes[@(address)]) return self.pipes[@(address)];
        if (!self.interfaces.count) [self openInterfaces];
        for (NSNumber *n in self.interfaces) {
            IOUSBHostPipe *pipe=[self.interfaces[n] copyPipeWithAddress:address error:NULL];
            if (pipe) { self.pipes[@(address)]=pipe; self.owners[@(address)]=n; return pipe; }
        }
        return nil;
    }
}
@end

void *fabo_usb_capture(uint32_t location,int fd,char **error) {
    @autoreleasepool {
        io_iterator_t iterator=0; io_service_t service=0,found=0;
        if (IOServiceGetMatchingServices(kIOMainPortDefault,IOServiceMatching("IOUSBHostDevice"),&iterator)!=KERN_SUCCESS) {
            describe(error,@"the I/O Registry did not answer");return NULL;
        }
        while ((service=IOIteratorNext(iterator))) {
            if (number(service,@"locationID")==location && number(service,@"bDeviceClass")!=9) {found=service;break;}
            IOObjectRelease(service);
        }
        IOObjectRelease(iterator);
        if (!found) {describe(error,@"no USB device at that location");return NULL;}
        FaboUSBDevice *host=[FaboUSBDevice new];host.service=found;host.fd=fd;
        host.interfaces=[NSMutableDictionary dictionary];host.pipes=[NSMutableDictionary dictionary];host.owners=[NSMutableDictionary dictionary];
        __weak FaboUSBDevice *weakHost=host;
        NSError *err=nil;
        host.device=[[IOUSBHostDevice alloc] initWithIOService:found options:IOUSBHostObjectInitOptionsDeviceCapture queue:nil error:&err interestHandler:^(IOUSBHostObject *obj,uint32_t message,void *arg) {
            FaboUSBDevice *h=weakHost;
            if (h && message==kIOMessageServiceIsTerminated) {h.gone=YES;int socket=h.fd;if(socket>=0)shutdown(socket,SHUT_RDWR);}
        }];
        if (!host.device) {IOObjectRelease(found);describe(error,err.localizedDescription);return NULL;}
        return (__bridge_retained void *)host;
    }
}
static int32_t finish(BOOL ok,NSError *error,NSMutableData *data,void *buffer,size_t length,NSUInteger done,BOOL input,size_t *actual) {
    if (!ok && (uint32_t)error.code!=0xe00002e7) return failure(error);
    *actual=MIN(done,length);if(input && *actual)memcpy(buffer,data.bytes,*actual);return 0;
}
int32_t fabo_usb_control(void *handle,uint8_t type,uint8_t request,uint16_t value,uint16_t index,uint16_t length,void *buffer,size_t size,size_t *actual) {
    @autoreleasepool {
        FaboUSBDevice *host=(__bridge FaboUSBDevice *)handle;if(host.gone||host.closed)return -19;
        IOUSBDeviceRequest setup={type,request,value,index,length};BOOL input=(type&0x80)!=0;
        NSMutableData *data=input?[NSMutableData dataWithLength:length]:[NSMutableData dataWithBytes:buffer length:size];
        NSUInteger done=0;NSError *error=nil;
        BOOL ok=[host.device sendDeviceRequest:setup data:data bytesTransferred:&done completionTimeout:5 error:&error];
        return finish(ok,error,data,buffer,size,done,input,actual);
    }
}
int32_t fabo_usb_transfer(void *handle,uint8_t endpoint,void *buffer,size_t length,size_t *actual) {
    @autoreleasepool {
        FaboUSBDevice *host=(__bridge FaboUSBDevice *)handle;if(host.gone||host.closed)return -19;
        IOUSBHostPipe *pipe=[host pipe:endpoint];if(!pipe)return -32;
        BOOL input=(endpoint&0x80)!=0;NSMutableData *data=input?[NSMutableData dataWithLength:length]:[NSMutableData dataWithBytes:buffer length:length];
        NSUInteger done=0;NSError *error=nil;
        BOOL ok=[pipe sendIORequestWithData:data bytesTransferred:&done completionTimeout:0 error:&error];
        return finish(ok,error,data,buffer,length,done,input,actual);
    }
}
int32_t fabo_usb_configure(void *handle,uint8_t value) {
    @autoreleasepool {FaboUSBDevice *host=(__bridge FaboUSBDevice *)handle;[host closeInterfaces];NSError *e=nil;
        BOOL ok=[host.device configureWithValue:value matchInterfaces:NO error:&e];if(ok&&value)[host openInterfaces];return ok?0:failure(e);}
}
int32_t fabo_usb_alternate(void *handle,uint8_t number,uint8_t alt) {
    @autoreleasepool {FaboUSBDevice *host=(__bridge FaboUSBDevice *)handle;[host openInterfaces];IOUSBHostInterface *interface;
        @synchronized(host) {interface=host.interfaces[@(number)];for(NSNumber *ep in host.owners.allKeys){if([host.owners[ep] isEqual:@(number)]){[host.pipes removeObjectForKey:ep];[host.owners removeObjectForKey:ep];}}}
        if(!interface)return -32;NSError *e=nil;return [interface selectAlternateSetting:alt error:&e]?0:failure(e);}
}
int32_t fabo_usb_clear(void *handle,uint8_t endpoint) {
    @autoreleasepool {IOUSBHostPipe *pipe=[(__bridge FaboUSBDevice *)handle pipe:endpoint];if(!pipe)return -32;NSError *e=nil;return [pipe clearStallWithError:&e]?0:failure(e);}
}
void fabo_usb_abort(void *handle,uint8_t endpoint) {
    @autoreleasepool {FaboUSBDevice *host=(__bridge FaboUSBDevice *)handle;
        if(!endpoint){[host.device abortDeviceRequestsWithOption:IOUSBHostAbortOptionSynchronous error:NULL];return;}
        IOUSBHostPipe *pipe;@synchronized(host){pipe=host.pipes[@(endpoint)];}[pipe abortWithOption:IOUSBHostAbortOptionSynchronous error:NULL];}
}
void fabo_usb_close(void *handle) {
    @autoreleasepool {FaboUSBDevice *host=(__bridge FaboUSBDevice *)handle;host.closed=YES;NSArray *pipes;
        @synchronized(host){pipes=host.pipes.allValues;}for(IOUSBHostPipe *pipe in pipes)[pipe abortWithOption:IOUSBHostAbortOptionSynchronous error:NULL];
        [host.device abortDeviceRequestsWithOption:IOUSBHostAbortOptionSynchronous error:NULL];}
}
void fabo_usb_dispose(void *handle) {
    @autoreleasepool {FaboUSBDevice *host=(__bridge_transfer FaboUSBDevice *)handle;host.fd=-1;[host closeInterfaces];
        if(!host.gone)[host.device resetWithError:NULL];[host.device destroy];IOObjectRelease(host.service);}
}
