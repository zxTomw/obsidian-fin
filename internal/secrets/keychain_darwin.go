//go:build darwin && cgo

package secrets

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

static CFMutableDictionaryRef bank_query(const char *ref) {
    CFMutableDictionaryRef q = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFStringRef service = CFSTR("obsidian-fin");
    CFStringRef account = CFStringCreateWithCString(NULL, ref, kCFStringEncodingUTF8);
    CFDictionarySetValue(q, kSecClass, kSecClassGenericPassword);
    CFDictionarySetValue(q, kSecAttrService, service);
    CFDictionarySetValue(q, kSecAttrAccount, account);
    CFDictionarySetValue(q, kSecAttrSynchronizable, kCFBooleanFalse);
    CFRelease(account);
    return q;
}
static OSStatus bank_get(const char *ref, unsigned char **out, long *length) {
    CFMutableDictionaryRef q = bank_query(ref);
    CFDictionarySetValue(q, kSecReturnData, kCFBooleanTrue);
    CFDictionarySetValue(q, kSecMatchLimit, kSecMatchLimitOne);
    CFTypeRef result = NULL;
    OSStatus status = SecItemCopyMatching(q, &result);
    if (status == errSecSuccess && result != NULL) {
        CFDataRef data = (CFDataRef)result;
        *length = CFDataGetLength(data);
        *out = malloc(*length > 0 ? *length : 1);
        if (*out == NULL) { status = errSecAllocate; }
        else { memcpy(*out, CFDataGetBytePtr(data), *length); }
    }
    if (result != NULL) CFRelease(result);
    CFRelease(q);
    return status;
}
static OSStatus bank_set(const char *ref, const unsigned char *bytes, long length) {
    CFMutableDictionaryRef q = bank_query(ref);
    CFDataRef data = CFDataCreate(NULL, bytes, length);
    CFMutableDictionaryRef attrs = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFDictionarySetValue(attrs, kSecValueData, data);
    OSStatus status = SecItemUpdate(q, attrs);
    if (status == errSecItemNotFound) {
        CFDictionarySetValue(q, kSecValueData, data);
        CFDictionarySetValue(q, kSecAttrAccessible, kSecAttrAccessibleWhenUnlockedThisDeviceOnly);
        status = SecItemAdd(q, NULL);
    }
    CFRelease(attrs); CFRelease(data); CFRelease(q);
    return status;
}
static OSStatus bank_delete(const char *ref) {
    CFMutableDictionaryRef q = bank_query(ref);
    OSStatus status = SecItemDelete(q); CFRelease(q); return status;
}
*/
import "C"

import (
	"context"
	"unsafe"
)

type Keychain struct{}

func NewKeychain() Store { return Keychain{} }
func (Keychain) Get(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r := C.CString(ref)
	defer C.free(unsafe.Pointer(r))
	var out *C.uchar
	var size C.long
	status := C.bank_get(r, &out, &size)
	if status == C.errSecItemNotFound {
		return "", ErrNotFound
	}
	if status != C.errSecSuccess {
		return "", ErrUnavailable
	}
	defer C.free(unsafe.Pointer(out))
	return string(C.GoBytes(unsafe.Pointer(out), C.int(size))), nil
}
func (Keychain) Set(ctx context.Context, ref, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := C.CString(ref)
	defer C.free(unsafe.Pointer(r))
	v := C.CBytes([]byte(value))
	defer C.free(v)
	if C.bank_set(r, (*C.uchar)(v), C.long(len(value))) != C.errSecSuccess {
		return ErrUnavailable
	}
	return nil
}
func (Keychain) Delete(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := C.CString(ref)
	defer C.free(unsafe.Pointer(r))
	status := C.bank_delete(r)
	if status != C.errSecSuccess && status != C.errSecItemNotFound {
		return ErrUnavailable
	}
	return nil
}
