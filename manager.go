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

func findNextFrame(data []byte, startPos int) int {
	for i := startPos; i < len(data)-1; i++ {
		if data[i] == 0xFF && (data[i+1] == 0xFB || data[i+1] == 0xFA) {
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

		nextFrameStart := findNextFrame(data, frameStart+2)
		if nextFrameStart == -1 {
			frames = append(frames, MP3Frame{data: data[frameStart:]})
			break
		}

		frames = append(frames, MP3Frame{data: data[frameStart:nextFrameStart]})
		pos = nextFrameStart
	}

	return frames
}
