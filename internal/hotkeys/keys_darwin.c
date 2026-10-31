//go:build darwin && cgo

#include <CoreGraphics/CoreGraphics.h>
#include <Carbon/Carbon.h>
#include "keys_darwin.h"

enum {
    firstModifierKeycode = 0x36,
    lastModifierKeycode  = 0x3F,
    virtualKeycodeCount  = 128
};

int onlyCtrlShiftHeld(void) {
    CGEventFlags flags = CGEventSourceFlagsState(kCGEventSourceStateHIDSystemState);
    int ctrl  = (flags & kCGEventFlagMaskControl) != 0;
    int shift = (flags & kCGEventFlagMaskShift) != 0;
    int cmd   = (flags & kCGEventFlagMaskCommand) != 0;
    int alt   = (flags & kCGEventFlagMaskAlternate) != 0;
    return ctrl && shift && !cmd && !alt;
}

int anyNonModifierKeyDown(void) {
    for (int k = 0; k < virtualKeycodeCount; k++) {
        int isModifier = k >= firstModifierKeycode && k <= lastModifierKeycode;
        if (isModifier) continue;
        if (CGEventSourceKeyState(kCGEventSourceStateHIDSystemState, (CGKeyCode)k)) {
            return 1;
        }
    }
    return 0;
}
