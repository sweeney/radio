package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func NewEpisodeManager() *EpisodeManager {
	return &EpisodeManager{
		episodes: make([]Episode, 0),
	}
}

func (em *EpisodeManager) LoadFeed(feedPath string) error {
	log.Printf("Loading podcast feed from %s", feedPath)

	data, err := os.ReadFile(feedPath)
	if err != nil {
		return fmt.Errorf("failed to read feed file: %v", err)
	}

	var rss Rss
	if err := xml.Unmarshal(data, &rss); err != nil {
		return fmt.Errorf("failed to parse feed: %v", err)
	}

	em.mutex.Lock()
	defer em.mutex.Unlock()

	for _, item := range rss.Channel.Episodes {
		if item.Enclosure.URL == "" {
			log.Printf("Warning: Episode '%s' has no enclosure URL", item.Title)
			continue
		}
		if item.Enclosure.Type != "audio/mpeg" {
			log.Printf("Warning: Episode '%s' is not MP3 format", item.Title)
			continue
		}

		em.episodes = append(em.episodes, Episode{
			item: item,
		})
	}

	log.Printf("Loaded %d episodes from feed", len(em.episodes))
	return nil
}

func (em *EpisodeManager) downloadWithRetry(url string, maxRetries int) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		client := &http.Client{
			Timeout: 30 * time.Second,
		}

		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			log.Printf("Download attempt %d failed: %v", attempt, err)
			time.Sleep(time.Second * time.Duration(attempt))
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("server returned status %d", resp.StatusCode)
			log.Printf("Download attempt %d failed: %v", attempt, lastErr)
			time.Sleep(time.Second * time.Duration(attempt))
			continue
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = err
			log.Printf("Reading response body attempt %d failed: %v", attempt, err)
			time.Sleep(time.Second * time.Duration(attempt))
			continue
		}

		return data, nil
	}
	return nil, fmt.Errorf("all download attempts failed: %v", lastErr)
}

func parseMP3Header(data []byte) (*MP3Header, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("header too short")
	}

	// First 11 bits should be 1s (frame sync)
	header := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if (header >> 21) != frameSyncBits>>5 {
		return nil, fmt.Errorf("invalid frame sync")
	}

	// Extract key information from header
	version := (header >> 19) & 3
	layer := (header >> 17) & 3
	bitrateIndex := (header >> 12) & 0xF
	samplerateIndex := (header >> 10) & 3
	padding := (header >> 9) & 1

	// Validate header fields
	if version == 1 || layer == 0 {
		return nil, fmt.Errorf("invalid version/layer")
	}

	// Get bitrate from lookup table
	bitrate := mp3Bitrates[version][layer][bitrateIndex]
	if bitrate == 0 {
		return nil, fmt.Errorf("invalid bitrate")
	}

	// Get sample rate from lookup table
	sampleRate := mp3SampleRates[version][samplerateIndex]
	if sampleRate == 0 {
		return nil, fmt.Errorf("invalid sample rate")
	}

	// Calculate frame size
	var frameSize int
	if layer == 3 { // Layer 1
		frameSize = (12*bitrate*1000/sampleRate + int(padding)) * 4
	} else { // Layer 2 & 3
		frameSize = 144*bitrate*1000/sampleRate + int(padding)
	}

	return &MP3Header{
		frameSize:  frameSize,
		bitrate:    bitrate,
		sampleRate: sampleRate,
		padding:    padding == 1,
	}, nil
}

func findNextFrame(data []byte, startPos int) (frameStart int, frameSize int) {
	for i := startPos; i < len(data)-minFrameSize; i++ {
		// Look for frame sync pattern
		if data[i] == 0xFF && (data[i+1]&0xE0) == 0xE0 {
			header, err := parseMP3Header(data[i:])
			if err != nil {
				continue
			}

			// Verify we have enough data for the full frame
			if i+header.frameSize > len(data) {
				return -1, 0
			}

			// Verify next frame starts with sync pattern
			nextFramePos := i + header.frameSize
			if nextFramePos+1 < len(data) {
				if data[nextFramePos] == 0xFF && (data[nextFramePos+1]&0xE0) == 0xE0 {
					return i, header.frameSize
				}
			} else {
				// Last frame in data
				return i, header.frameSize
			}
		}
	}
	return -1, 0
}

func extractFrames(data []byte) []MP3Frame {
	var frames []MP3Frame
	pos := 0

	for pos < len(data) {
		frameStart, frameSize := findNextFrame(data, pos)
		if frameStart == -1 || frameSize == 0 {
			break
		}

		frames = append(frames, MP3Frame{
			data: data[frameStart : frameStart+frameSize],
		})
		pos = frameStart + frameSize
	}

	return frames
}

func (em *EpisodeManager) downloadEpisode(index int) error {
	em.mutex.Lock()
	if em.downloading {
		em.mutex.Unlock()
		return nil
	}
	em.downloading = true
	em.mutex.Unlock()

	defer func() {
		em.mutex.Lock()
		em.downloading = false
		em.mutex.Unlock()
	}()

	episode := &em.episodes[index]
	log.Printf("Downloading episode: %s", episode.item.Title)

	data, err := em.downloadWithRetry(episode.item.Enclosure.URL, 3)
	if err != nil {
		return fmt.Errorf("failed to download episode after retries: %v", err)
	}

	frames := extractFrames(data)
	if len(frames) == 0 {
		return fmt.Errorf("no valid MP3 frames found in episode")
	}

	em.mutex.Lock()
	episode.data = data
	episode.frames = frames
	episode.position = 0
	em.mutex.Unlock()

	log.Printf("Successfully downloaded episode: %s (%d frames)", episode.item.Title, len(frames))
	return nil
}

func (em *EpisodeManager) StartDownloading(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				em.mutex.RLock()
				currentIdx := em.currentIndex
				nextIdx := (currentIdx + 1) % len(em.episodes)
				downloading := em.downloading
				em.mutex.RUnlock()

				if !downloading && em.episodes[nextIdx].data == nil {
					if err := em.downloadEpisode(nextIdx); err != nil {
						log.Printf("Error downloading next episode: %v", err)
						time.Sleep(time.Second * 5)
						continue
					}
				}

				time.Sleep(time.Second)
			}
		}
	}()
}
