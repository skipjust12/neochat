package attachment

import "encoding/binary"

// VideoDuration reads an MP4 or QuickTime file's length from its movie
// header (moov/mvhd), wherever the moov box sits. ok is false for other
// containers (WebM) or a file it can't make sense of.
func VideoDuration(data []byte) (seconds float64, ok bool) {
	moov, found := findBox(data, "moov")
	if !found {
		return 0, false
	}
	mvhd, found := findBox(moov, "mvhd")
	if !found || len(mvhd) < 4 {
		return 0, false
	}
	var timescale uint32
	var duration uint64
	switch version := mvhd[0]; version {
	case 0:
		if len(mvhd) < 20 {
			return 0, false
		}
		timescale = binary.BigEndian.Uint32(mvhd[12:16])
		duration = uint64(binary.BigEndian.Uint32(mvhd[16:20]))
	case 1:
		if len(mvhd) < 32 {
			return 0, false
		}
		timescale = binary.BigEndian.Uint32(mvhd[20:24])
		duration = binary.BigEndian.Uint64(mvhd[24:32])
	default:
		return 0, false
	}
	if timescale == 0 || duration == 0 || duration == ^uint64(0) || duration == uint64(^uint32(0)) {
		return 0, false
	}
	return float64(duration) / float64(timescale), true
}

// findBox returns the payload of the first box of the given type among
// the boxes laid out back to back in data.
func findBox(data []byte, boxType string) ([]byte, bool) {
	for len(data) >= 8 {
		size := uint64(binary.BigEndian.Uint32(data[:4]))
		header := uint64(8)
		switch size {
		case 0: // to the end of the data
			size = uint64(len(data))
		case 1: // 64-bit size after the type
			if len(data) < 16 {
				return nil, false
			}
			size = binary.BigEndian.Uint64(data[8:16])
			header = 16
		}
		if size < header || size > uint64(len(data)) {
			return nil, false
		}
		if string(data[4:8]) == boxType {
			return data[header:size], true
		}
		data = data[size:]
	}
	return nil, false
}
