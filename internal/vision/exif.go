package vision

import (
	"bytes"
	"encoding/binary"
)

// Only the bounded IFD0 orientation tag is inspected. Malformed or unrelated
// metadata leaves decoded pixels unchanged; image decoding validates the image.
func tiffOrientation(raw []byte) int {
	if bytes.HasPrefix(raw, []byte("Exif\x00\x00")) {
		raw = raw[6:]
	}
	if len(raw) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(raw[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(raw[2:4]) != 42 {
		return 1
	}
	offset := uint64(order.Uint32(raw[4:8]))
	if offset+2 > uint64(len(raw)) {
		return 1
	}
	count := uint64(order.Uint16(raw[offset : offset+2]))
	offset += 2
	if offset+12*count > uint64(len(raw)) {
		return 1
	}
	for i := uint64(0); i < count; i++ {
		entry := raw[offset+12*i : offset+12*(i+1)]
		if order.Uint16(entry[:2]) == 0x112 && order.Uint16(entry[2:4]) == 3 && order.Uint32(entry[4:8]) == 1 {
			n := int(order.Uint16(entry[8:10]))
			if n >= 1 && n <= 8 {
				return n
			}
			return 1
		}
	}
	return 1
}

func exifOrientation(raw []byte, format string) int {
	switch format {
	case "jpeg":
		for p := 2; p+1 < len(raw); {
			if raw[p] != 0xff {
				return 1
			}
			for p < len(raw) && raw[p] == 0xff {
				p++
			}
			if p >= len(raw) {
				return 1
			}
			marker := raw[p]
			p++
			if marker == 0xda || marker == 0xd9 {
				return 1
			}
			if marker == 0x01 || marker >= 0xd0 && marker <= 0xd8 {
				continue
			}
			if p+2 > len(raw) {
				return 1
			}
			n := int(binary.BigEndian.Uint16(raw[p : p+2]))
			if n < 2 || n > len(raw)-p {
				return 1
			}
			payload := raw[p+2 : p+n]
			p += n
			if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				return tiffOrientation(payload)
			}
		}
	case "png":
		for p := 8; p+12 <= len(raw); {
			n := uint64(binary.BigEndian.Uint32(raw[p : p+4]))
			if n > uint64(len(raw)-p-12) {
				return 1
			}
			if string(raw[p+4:p+8]) == "eXIf" {
				return tiffOrientation(raw[p+8 : p+8+int(n)])
			}
			p += 12 + int(n)
		}
	case "webp":
		for p := 12; p+8 <= len(raw); {
			n := uint64(binary.LittleEndian.Uint32(raw[p+4 : p+8]))
			if n > uint64(len(raw)-p-8) {
				return 1
			}
			if string(raw[p:p+4]) == "EXIF" {
				return tiffOrientation(raw[p+8 : p+8+int(n)])
			}
			p += 8 + int(n) + int(n&1)
		}
	}
	return 1
}
