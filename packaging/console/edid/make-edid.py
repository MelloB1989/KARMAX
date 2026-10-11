#!/usr/bin/env python3
"""Writes a 128-byte EDID 1.3 block for one fixed mode to stdout.

    make-edid.py 1920 1080 60 [--name NAME] > fleet-1920x1080.bin

The timing is CVT reduced blanking v1, which every digital sink and the kernel's
EDID parser accept; the checksum byte makes the block sum to 0 mod 256.
"""
import argparse
import sys

MANUFACTURER = "FLT"  # three letters, not a registered PNP id: it only labels the virtual output


def cvt_rb(w, h, hz):
    """CVT-RB v1 timing: returns pixel clock (10 kHz units) and the porch/sync/blank values."""
    h_blank, h_fp, h_sync = 160, 48, 32
    ratio = w / h
    v_sync = 5 if abs(ratio - 16 / 9) < 0.02 else 4 if abs(ratio - 4 / 3) < 0.02 else 6
    v_fp, min_bp = 3, 6
    h_period_est = (1_000_000 / hz - 460) / h
    vbi = max(int(460 / h_period_est) + 1, v_fp + v_sync + min_bp)
    v_total = h + vbi
    clock_khz = int((hz * v_total * (w + h_blank) / 1000) // 250 * 250)  # round down to 0.25 MHz
    return {"clock": clock_khz // 10, "h_blank": h_blank, "h_fp": h_fp, "h_sync": h_sync,
            "v_blank": vbi, "v_fp": v_fp, "v_sync": v_sync, "v_total": v_total}


def detailed_timing(w, h, hz, mm_w, mm_h):
    t = cvt_rb(w, h, hz)
    d = bytearray(18)
    d[0], d[1] = t["clock"] & 0xFF, t["clock"] >> 8
    d[2], d[3] = w & 0xFF, t["h_blank"] & 0xFF
    d[4] = ((w >> 8) << 4) | (t["h_blank"] >> 8)
    d[5], d[6] = h & 0xFF, t["v_blank"] & 0xFF
    d[7] = ((h >> 8) << 4) | (t["v_blank"] >> 8)
    d[8], d[9] = t["h_fp"] & 0xFF, t["h_sync"] & 0xFF
    d[10] = ((t["v_fp"] & 0xF) << 4) | (t["v_sync"] & 0xF)
    d[11] = ((t["h_fp"] >> 8) << 6) | ((t["h_sync"] >> 8) << 4) | ((t["v_fp"] >> 4) << 2) | (t["v_sync"] >> 4)
    d[12], d[13] = mm_w & 0xFF, mm_h & 0xFF
    d[14] = ((mm_w >> 8) << 4) | (mm_h >> 8)
    d[15] = d[16] = 0
    d[17] = 0x1A  # digital separate sync, +hsync, -vsync (reduced blanking)
    return bytes(d), t


def text_descriptor(tag, text):
    body = (text[:12] + "\n").ljust(13).encode("ascii")
    return bytes([0, 0, 0, tag, 0]) + body


def range_descriptor(clock):
    v_min, v_max = 24, 75
    h_min, h_max = 15, 90
    max_clock = min(255, -(-clock // 1000) + 1)  # in 10 MHz units
    d = bytes([0, 0, 0, 0xFD, 0, v_min, v_max, h_min, h_max, max_clock, 0x01, 0x0A] + [0x20] * 6)
    return d


def make_edid(w, h, hz, name="FLEET-VIRT"):
    if not (640 <= w <= 4095 and 480 <= h <= 4095 and 24 <= hz <= 75):
        raise ValueError(f"unsupported mode {w}x{h}@{hz}")
    mm_w = round(w * 0.2735)  # about a 24-inch panel's pixel pitch
    mm_h = round(h * 0.2735)
    b = bytearray(128)
    b[0:8] = bytes([0, 255, 255, 255, 255, 255, 255, 0])
    m = [ord(c) - 64 for c in MANUFACTURER]
    b[8] = (m[0] << 2) | (m[1] >> 3)
    b[9] = ((m[1] & 7) << 5) | m[2]
    b[10], b[11] = 0x01, 0x00  # product code
    b[12:16] = bytes([1, 0, 0, 0])  # serial
    b[16], b[17] = 1, 35  # week 1, year 2025
    b[18], b[19] = 1, 3  # EDID 1.3
    b[20] = 0x80  # digital input
    b[21], b[22] = min(255, mm_w // 10), min(255, mm_h // 10)
    b[23] = 0x78  # gamma 2.2
    b[24] = 0x0A  # RGB 4:4:4, preferred timing is the first descriptor
    b[25:35] = bytes([0xEE, 0x91, 0xA3, 0x54, 0x4C, 0x99, 0x26, 0x0F, 0x50, 0x54])  # sRGB primaries
    b[35:38] = bytes([0, 0, 0])  # no established timings
    for i in range(8):  # unused standard timings
        b[38 + 2 * i], b[39 + 2 * i] = 0x01, 0x01
    dtd, t = detailed_timing(w, h, hz, mm_w, mm_h)
    b[54:72] = dtd
    b[72:90] = text_descriptor(0xFC, name)
    b[90:108] = range_descriptor(t["clock"])
    b[108:126] = bytes([0, 0, 0, 0x10, 0] + [0] * 13)  # dummy
    b[126] = 0  # no extensions
    b[127] = (256 - sum(b[:127]) % 256) % 256
    return bytes(b)


def main(argv):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("width", type=int)
    p.add_argument("height", type=int)
    p.add_argument("hz", type=int)
    p.add_argument("--name", default="FLEET-VIRT")
    a = p.parse_args(argv)
    try:
        sys.stdout.buffer.write(make_edid(a.width, a.height, a.hz, a.name))
    except ValueError as e:
        print(f"make-edid: {e}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
