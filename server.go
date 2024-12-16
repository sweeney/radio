package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

func NewStreamServer(defaultEpisodeLimit int) *StreamServer {
	return &StreamServer{
		episodeManager:      NewEpisodeManager(),
		clients:             make(map[chan struct{}]*Client),
		addresses:           make(map[string]int),
		shutdown:            make(chan struct{}),
		defaultEpisodeLimit: defaultEpisodeLimit,
	}
}

func (s *StreamServer) AddClient(addr string, episodeLimit int) chan struct{} {
	log.Printf("Add Client given limit=%d", episodeLimit)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Store the original passed limit before potentially modifying it
	actualLimit := episodeLimit
	if actualLimit == 0 {
		actualLimit = s.defaultEpisodeLimit
	}

	log.Printf("using limit=%d", actualLimit)

	ch := make(chan struct{}, 1)
	s.clients[ch] = &Client{
		ch:           ch,
		address:      addr,
		lastSeen:     time.Now(),
		episodeCount: 0,
		maxEpisodes:  actualLimit,
	}
	s.addresses[addr] = s.addresses[addr] + 1

	uniqueClients := len(s.addresses)
	totalConns := 0
	for _, count := range s.addresses {
		totalConns += count
	}

	log.Printf("New client connected from %s (limit: %d episodes, connections for this client: %d, unique clients: %d, total connections: %d)",
		addr, actualLimit, s.addresses[addr], uniqueClients, totalConns)

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

func (s *StreamServer) incrementEpisodeCountsAndCheckLimits() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	disconnectChannels := make([]chan struct{}, 0)

	for ch, client := range s.clients {
		client.episodeCount++

		if client.maxEpisodes > 0 && client.episodeCount >= client.maxEpisodes {
			log.Printf("Client %s reached episode limit (%d), marking for disconnect",
				client.address, client.maxEpisodes)
			disconnectChannels = append(disconnectChannels, ch)
		}
	}

	for _, ch := range disconnectChannels {
		s.RemoveClient(ch)
	}
}

func (s *StreamServer) StartStreaming(feedPath string) {
	if err := s.episodeManager.LoadFeed(feedPath); err != nil {
		log.Fatalf("Error loading podcast feed: %v", err)
	}

	if len(s.episodeManager.episodes) == 0 {
		log.Fatal("No valid episodes found in feed")
	}

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
				nextIndex := (s.episodeManager.currentIndex + 1) % len(s.episodeManager.episodes)

				if s.episodeManager.episodes[nextIndex].data == nil {
					s.episodeManager.mutex.RUnlock()
					log.Printf("Waiting for next episode to download...")
					time.Sleep(time.Second)
					continue
				}

				s.NotifyClients()
				time.Sleep(frameTiming)
				currentEpisode.position = 0

				if s.episodeManager.currentIndex > 0 {
					prevIndex := (s.episodeManager.currentIndex - 1)
					s.episodeManager.episodes[prevIndex].data = nil
					s.episodeManager.episodes[prevIndex].frames = nil
				}

				s.incrementEpisodeCountsAndCheckLimits()

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
	episodeLimit := flag.Int("episode-limit", 3, "Default number of episodes before client disconnect (-1 for unlimited)")
	flag.Parse()

	log.Printf("Startup limit=%d", *episodeLimit)

	if _, err := os.Stat(*feedPath); os.IsNotExist(err) {
		log.Fatalf("Feed file does not exist: %s", *feedPath)
	}

	server := NewStreamServer(*episodeLimit)

	mux := http.NewServeMux()
	mux.HandleFunc("/listen", func(w http.ResponseWriter, r *http.Request) {

		log.Printf("Full URL: %s", r.URL.String())
		log.Printf("Raw Query: %s", r.URL.RawQuery)
		log.Printf("Path: %s", r.URL.Path)
		log.Printf("All Query Parameters: %v", r.URL.Query())

		limit := 0 // Will use default if 0
		if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
			log.Printf("limitStr=%s", limitStr)

			if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit >= -1 {

				limit = parsedLimit
				log.Printf("parsedlimit=%d", limit)

			}
		}

		done := make(chan struct{})
		go func() {
			<-r.Context().Done()
			close(done)
		}()

		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		w.Header().Set("icy-br", "128")
		w.Header().Set("ice-audio-info", "channels=2;samplerate=44100;bitrate=128")
		w.Header().Set("icy-name", "Radio Sween")

		clientCh := server.AddClient(getClientIP(r), limit)
		defer server.RemoveClient(clientCh)

		for {
			select {
			case <-clientCh:
				frame := server.GetCurrentFrame()
				if frame != nil {
					if _, err := w.Write(frame); err != nil {
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
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      mux,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
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
