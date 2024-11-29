package main

import (
	"fmt"
)

// Bitrate lookup table (MPEG1, Layer III)
var bitrateTable = map[int]int{
	0x1: 32000,
	0x2: 40000,
	0x3: 48000,
	0x4: 56000,
	0x5: 64000,
	0x6: 80000,
	0x7: 96000,
	0x8: 112000,
	0x9: 128000,
	0xa: 160000,
	0xb: 192000,
	0xc: 224000,
	0xd: 256000,
	0xe: 320000,
}

// Sample rate lookup table (MPEG1)
var sampleRateTable = map[int]int{
	0x0: 44100,
	0x1: 48000,
	0x2: 32000,
}

func parseMP3Header(frameData []byte) (MP3Header, error) {
	if len(frameData) < 4 {
		return MP3Header{}, fmt.Errorf("frame too short")
	}

	// First 11 bits must be set for valid frame sync
	if frameData[0] != 0xff || (frameData[1]&0xe0) != 0xe0 {
		return MP3Header{}, fmt.Errorf("invalid frame sync")
	}

	// Extract bitrate index (bits 16-19)
	bitrateIndex := int((frameData[2] >> 4) & 0x0f)
	bitrate := bitrateTable[bitrateIndex]

	// Extract sample rate index (bits 20-21)
	sampleRateIndex := int((frameData[2] >> 2) & 0x03)
	sampleRate := sampleRateTable[sampleRateIndex]

	// Extract padding bit (bit 22)
	padding := int((frameData[2] >> 1) & 0x01)

	// Calculate frame size
	// Frame size = (144 * bitrate) / sample rate + padding
	frameSize := (144 * bitrate / sampleRate) + padding

	return MP3Header{
		bitrate:    bitrate,
		sampleRate: sampleRate,
		padding:    padding,
		frameSize:  frameSize,
	}, nil
}

func findNextFrame(data []byte, startPos int) int {
	for i := startPos; i < len(data)-1; i++ {
		if data[i] == 0xff && (data[i+1]&0xe0) == 0xe0 {
			return i
		}
	}
	return -1
}

func extractFrames(data []byte) []MP3Frame {
	var frames []MP3Frame
	pos := 0

	for {
		frameStart := findNextFrame(data, pos)
		if frameStart == -1 || frameStart >= len(data)-4 {
			break
		}

		// Parse the header
		header, err := parseMP3Header(data[frameStart:])
		if err != nil {
			pos = frameStart + 1
			continue
		}

		// Use the frame size from the header
		frameEnd := frameStart + header.frameSize
		if frameEnd > len(data) {
			break
		}

		frames = append(frames, MP3Frame{
			data:   data[frameStart:frameEnd],
			header: header,
		})

		pos = frameEnd
	}

	return frames
}
