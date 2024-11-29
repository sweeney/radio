package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func getClientIP(r *http.Request) string {
	// Try X-Forwarded-For header first
	forwardedFor := r.Header.Get("X-Forwarded-For")
	if forwardedFor != "" {
		// X-Forwarded-For can contain multiple IPs, take the first one
		ips := strings.Split(forwardedFor, ",")
		return strings.TrimSpace(ips[0])
	}

	// Try X-Real-IP header next
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func NewStreamServer() *StreamServer {
	return &StreamServer{
		episodeManager: NewEpisodeManager(),
		clients:        make(map[chan struct{}]*Client),
		addresses:      make(map[string]int),
		shutdown:       make(chan struct{}),
	}
}

func (s *StreamServer) AddClient(addr string) chan struct{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	ch := make(chan struct{}, 1)
	s.clients[ch] = &Client{
		ch:       ch,
		address:  addr,
		lastSeen: time.Now(),
	}
	s.addresses[addr] = s.addresses[addr] + 1

	uniqueClients := len(s.addresses)
	totalConns := 0
	for _, count := range s.addresses {
		totalConns += count
	}

	log.Printf("New client connected from %s (connections for this client: %d, unique clients: %d, total connections: %d)",
		addr, s.addresses[addr], uniqueClients, totalConns)

	s.activeConns.Add(1)
	return ch
}

func (s *StreamServer) RemoveClient(ch chan struct{}) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if client, exists := s.clients[ch]; exists {
		addr := client.address
		s.addresses[addr]--
		if s.addresses[addr] <= 0 {
			delete(s.addresses, addr)
		}
		delete(s.clients, ch)
		close(ch)
		s.activeConns.Done()

		uniqueClients := len(s.addresses)
		totalConns := 0
		for _, count := range s.addresses {
			totalConns += count
		}

		log.Printf("Client disconnected from %s (connections remaining for this client: %d, unique clients: %d, total connections: %d)",
			addr, s.addresses[addr], uniqueClients, totalConns)
	}
}

func (s *StreamServer) NotifyClients() {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	for ch := range s.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *StreamServer) GetCurrentFrame() []byte {
	s.episodeManager.mutex.RLock()
	defer s.episodeManager.mutex.RUnlock()

	episode := &s.episodeManager.episodes[s.episodeManager.currentIndex]
	if episode.position >= len(episode.frames) {
		return nil
	}
	return episode.frames[episode.position].data
}

func (s *StreamServer) StartStreaming(feedPath string) {
	if err := s.episodeManager.LoadFeed(feedPath); err != nil {
		log.Fatalf("Error loading podcast feed: %v", err)
	}

	if len(s.episodeManager.episodes) == 0 {
		log.Fatal("No valid episodes found in feed")
	}

	// Start the background downloader
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.episodeManager.StartDownloading(ctx)

	// Download first episode before starting
	if err := s.episodeManager.downloadEpisode(0); err != nil {
		log.Fatalf("Error downloading first episode: %v", err)
	}

	frameTiming := time.Millisecond * 26

	for {
		select {
		case <-s.shutdown:
			log.Println("Stopping streaming...")
			return
		default:
			s.episodeManager.mutex.RLock()
			currentEpisode := &s.episodeManager.episodes[s.episodeManager.currentIndex]

			if currentEpisode.position >= len(currentEpisode.frames) {
				// Move to next episode
				currentEpisode.position = 0
				nextIndex := (s.episodeManager.currentIndex + 1) % len(s.episodeManager.episodes)

				// Make sure next episode is ready
				if s.episodeManager.episodes[nextIndex].data == nil {
					s.episodeManager.mutex.RUnlock()
					log.Printf("Waiting for next episode to download...")
					time.Sleep(time.Second)
					continue
				}

				// Clean up previous episode
				if s.episodeManager.currentIndex > 0 {
					prevIndex := (s.episodeManager.currentIndex - 1)
					s.episodeManager.episodes[prevIndex].data = nil
					s.episodeManager.episodes[prevIndex].frames = nil
				}

				s.episodeManager.currentIndex = nextIndex
				log.Printf("Moving to next episode: %s", s.episodeManager.episodes[nextIndex].item.Title)
				s.episodeManager.mutex.RUnlock()
				continue
			}

			s.NotifyClients()
			time.Sleep(frameTiming)
			currentEpisode.position++
			s.episodeManager.mutex.RUnlock()
		}
	}
}

func (s *StreamServer) Shutdown() {
	log.Println("Initiating shutdown...")

	close(s.shutdown)

	s.mutex.Lock()
	for ch := range s.clients {
		close(ch)
		delete(s.clients, ch)
	}
	s.mutex.Unlock()

	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All connections closed")
	case <-time.After(2 * time.Second):
		log.Println("Timeout waiting for connections to close")
	}

	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		if err := s.server.Shutdown(ctx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		} else {
			log.Println("HTTP server shutdown complete")
		}
	}

	log.Println("Shutdown complete")
}

func main() {
	feedPath := flag.String("feed", "pod.xml", "Path to the podcast RSS feed XML file")
	port := flag.Int("port", 3000, "Port to listen on")
	flag.Parse()

	if _, err := os.Stat(*feedPath); os.IsNotExist(err) {
		log.Fatalf("Feed file does not exist: %s", *feedPath)
	}

	server := NewStreamServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/listen", func(w http.ResponseWriter, r *http.Request) {
		done := make(chan struct{})

		go func() {
			<-r.Context().Done()
			close(done)
		}()

		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "no-cache, no-store")
//		w.Header().Set("icy-br", "128")
//		w.Header().Set("ice-audio-info", "channels=2;samplerate=44100;bitrate=128")
		w.Header().Set("icy-name", "Podcast Stream")

		clientCh := server.AddClient(getClientIP(r))
		defer server.RemoveClient(clientCh)

		for {
			select {
			case <-clientCh:
				frame := server.GetCurrentFrame()
				if frame != nil {
					_, err := w.Write(frame)
					if err != nil {
						return
					}
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			case <-done:
				return
			case <-server.shutdown:
				log.Printf("Server shutdown, closing client %s", getClientIP(r))
				return
			}
		}
	})

	server.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go server.StartStreaming(*feedPath)

	go func() {
		log.Printf("Starting server on port %d", *port)
		if err := server.server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sig := <-sigChan
	log.Printf("Received signal: %v", sig)
	server.Shutdown()
}
