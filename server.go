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

// getClientIP extracts the client's real IP address from the request
// It checks for common proxy headers before falling back to direct IP
func getClientIP(r *http.Request) string {
	// Try X-Forwarded-For header first - commonly set by proxies
	forwardedFor := r.Header.Get("X-Forwarded-For")
	if forwardedFor != "" {
		// X-Forwarded-For can contain multiple IPs in a chain, take the first (original) one
		ips := strings.Split(forwardedFor, ",")
		return strings.TrimSpace(ips[0])
	}

	// Try X-Real-IP header next - often set by nginx
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}

	// Fall back to remote address if no proxy headers found
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // Return full address if parsing fails
	}
	return ip
}

// NewStreamServer creates a new streaming server instance
// defaultEpisodeLimit sets how many episodes a client can listen to before disconnect
func NewStreamServer(defaultEpisodeLimit int) *StreamServer {
	return &StreamServer{
		episodeManager:      NewEpisodeManager(),
		clients:             make(map[chan struct{}]*Client),
		addresses:           make(map[string]int), // Tracks number of connections per IP
		shutdown:            make(chan struct{}),
		defaultEpisodeLimit: defaultEpisodeLimit,
	}
}

// AddClient registers a new client connection with the server
// Returns a channel that will be used to notify the client of new frames
func (s *StreamServer) AddClient(addr string, episodeLimit int) chan struct{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Use default limit if none specified
	actualLimit := episodeLimit
	if actualLimit == 0 {
		actualLimit = s.defaultEpisodeLimit
	}

	// Create notification channel for this client
	ch := make(chan struct{}, 1)
	s.clients[ch] = &Client{
		ch:           ch,
		address:      addr,
		lastSeen:     time.Now(),
		episodeCount: 0,
		maxEpisodes:  actualLimit,
	}

	// Track number of connections from this IP
	s.addresses[addr] = s.addresses[addr] + 1

	// Log connection statistics
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

// RemoveClient cleanly removes a client and performs cleanup
func (s *StreamServer) RemoveClient(ch chan struct{}) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if client, exists := s.clients[ch]; exists {
		addr := client.address

		// Signal termination first
		close(client.done)

		// Decrease connection count for this IP
		s.addresses[addr]--
		if s.addresses[addr] <= 0 {
			delete(s.addresses, addr)
		}
		delete(s.clients, ch)
		close(ch)
		s.activeConns.Done()

		// Log updated connection statistics
		uniqueClients := len(s.addresses)
		totalConns := 0
		for _, count := range s.addresses {
			totalConns += count
		}

		log.Printf("Client disconnected from %s (connections remaining for this client: %d, unique clients: %d, total connections: %d)",
			addr, s.addresses[addr], uniqueClients, totalConns)
	}
}

// NotifyClients sends a notification to all connected clients that a new frame is available
func (s *StreamServer) NotifyClients() {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	for ch := range s.clients {
		select {
		case ch <- struct{}{}: // Notify client
		default: // Skip if client's channel is full (they're processing slowly)
		}
	}
}

// GetCurrentFrame returns the current audio frame being played
// Returns nil if no frame is available
func (s *StreamServer) GetCurrentFrame() []byte {
	s.episodeManager.mutex.RLock()
	defer s.episodeManager.mutex.RUnlock()

	episode := &s.episodeManager.episodes[s.episodeManager.currentIndex]
	if episode.position >= len(episode.frames) {
		return nil
	}
	return episode.frames[episode.position].data
}

// incrementEpisodeCountsAndCheckLimits checks if any clients have reached their episode limits
// and disconnects them if necessary
func (s *StreamServer) incrementEpisodeCountsAndCheckLimits() {
	s.mutex.Lock()

	// Collect clients that need to be disconnected
	disconnectChannels := make([]chan struct{}, 0)

	for ch, client := range s.clients {
		client.episodeCount++

		if client.maxEpisodes > 0 && client.episodeCount >= client.maxEpisodes {
			log.Printf("Client %s reached episode limit (%d), marking for disconnect",
				client.address, client.maxEpisodes)
			disconnectChannels = append(disconnectChannels, ch)
		}
	}

	s.mutex.Unlock()

	// Process disconnections after releasing the lock to avoid deadlocks
	for _, ch := range disconnectChannels {
		s.RemoveClient(ch)
	}
}

// StartStreaming begins streaming episodes from the podcast feed
func (s *StreamServer) StartStreaming(feedPath string) {
	// Load and validate the feed
	if err := s.episodeManager.LoadFeed(feedPath); err != nil {
		log.Fatalf("Error loading podcast feed: %v", err)
	}

	if len(s.episodeManager.episodes) == 0 {
		log.Fatal("No valid episodes found in feed")
	}

	// Start background episode downloading
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.episodeManager.StartDownloading(ctx)

	// Ensure first episode is ready before starting
	if err := s.episodeManager.downloadEpisode(0); err != nil {
		log.Fatalf("Error downloading first episode: %v", err)
	}

	frameTiming := time.Millisecond * 26 // ~38fps for audio frames

	for {
		select {
		case <-s.shutdown:
			log.Println("Stopping streaming...")
			return
		default:
			s.episodeManager.mutex.RLock()
			currentEpisode := &s.episodeManager.episodes[s.episodeManager.currentIndex]

			// Check if we've reached the end of the current episode
			if currentEpisode.position >= len(currentEpisode.frames) {
				nextIndex := (s.episodeManager.currentIndex + 1) % len(s.episodeManager.episodes)

				// Wait if next episode isn't ready
				if s.episodeManager.episodes[nextIndex].data == nil {
					s.episodeManager.mutex.RUnlock()
					log.Printf("Waiting for next episode to download...")
					time.Sleep(time.Second)
					continue
				}

				s.NotifyClients()
				time.Sleep(frameTiming)
				currentEpisode.position = 0

				// Clean up previous episode to free memory
				if s.episodeManager.currentIndex > 0 {
					prevIndex := (s.episodeManager.currentIndex - 1)
					s.episodeManager.episodes[prevIndex].data = nil
					s.episodeManager.episodes[prevIndex].frames = nil
				}

				// Check if any clients have reached their episode limits
				s.incrementEpisodeCountsAndCheckLimits()

				s.episodeManager.currentIndex = nextIndex
				log.Printf("Moving to next episode: %s", s.episodeManager.episodes[nextIndex].item.Title)
				s.episodeManager.mutex.RUnlock()
				continue
			}

			// Send current frame to all clients
			s.NotifyClients()
			time.Sleep(frameTiming)
			currentEpisode.position++
			s.episodeManager.mutex.RUnlock()
		}
	}
}

// Global variable to hold our silence frames
var silentFrames []byte

// LoadSilentFrames loads the MP3 silence file at startup
func LoadSilentFrames() error {
	var err error
	silentFrames, err = os.ReadFile("./mp3/silence.mp3")
	if err != nil {
		return fmt.Errorf("failed to read silence.mp3: %v", err)
	}
	return nil
}

// sendTerminationFrames writes silent MP3 frames to gracefully end the stream
func sendTerminationFrames(w http.ResponseWriter) {
	if silentFrames == nil {
		log.Printf("Silent frames not loaded, skipping termination frames")
		return
	}

	w.Write(silentFrames)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Small delay to allow the frames to be processed
	log.Printf("Silence sent, small sleep...")
	time.Sleep(time.Millisecond * 100)
}

// handleStream manages an individual client's audio stream
// Ensures clean shutdown and proper resource cleanup
func handleStream(w http.ResponseWriter, r *http.Request, server *StreamServer, clientCh chan struct{}, done chan struct{}) {
	defer server.RemoveClient(clientCh)

	// Set response headers for MP3 streaming
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "close") // Ensure connection will be closed when done
	w.Header().Set("icy-br", "128")
	w.Header().Set("ice-audio-info", "channels=2;samplerate=44100;bitrate=128")
	w.Header().Set("icy-name", "Radio Sween")

	// Ensure headers are written before starting stream
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Main streaming loop
	for {
		select {
		case <-clientCh: // New frame available
			frame := server.GetCurrentFrame()
			if frame != nil {
				if _, err := w.Write(frame); err != nil {
					log.Printf("Write error for client %s: %v", getClientIP(r), err)
					// Ensure any buffered data is sent before returning
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					return
				}
				// Flush frame to client immediately
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		case <-done: // Client disconnected
			sendTerminationFrames(w)
			log.Printf("Client %s disconnected normally", getClientIP(r))
			return
		case <-server.shutdown: // Server is shutting down
			log.Printf("Server shutdown, closing client %s", getClientIP(r))
			sendTerminationFrames(w)
			return
		}
	}
}

// Shutdown gracefully shuts down the server and all client connections
func (s *StreamServer) Shutdown() {
	log.Println("Initiating shutdown...")

	close(s.shutdown)

	// Close all client connections
	s.mutex.Lock()
	for ch := range s.clients {
		close(ch)
		delete(s.clients, ch)
	}
	s.mutex.Unlock()

	// Wait for all connections to finish
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

	// Shutdown HTTP server if it exists
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
	// Parse command line flags
	feedPath := flag.String("feed", "pod.xml", "Path to the podcast RSS feed XML file")
	port := flag.Int("port", 3000, "Port to listen on")
	episodeLimit := flag.Int("episode-limit", 3, "Default number of episodes before client disconnect (-1 for unlimited)")
	flag.Parse()

	// Verify feed file exists
	if _, err := os.Stat(*feedPath); os.IsNotExist(err) {
		log.Fatalf("Feed file does not exist: %s", *feedPath)
	}

	// Load silent frames
	if err := LoadSilentFrames(); err != nil {
		log.Fatalf("Failed to load silence.mp3: %v", err)
	}

	// Create and initialize server
	server := NewStreamServer(*episodeLimit)

	// Set up HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/listen", func(w http.ResponseWriter, r *http.Request) {
		// Parse episode limit from query parameters
		limit := 0 // Will use default if 0
		if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
			if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit >= -1 {
				limit = parsedLimit
			}
		}

		// Create done channel to monitor client disconnection
		done := make(chan struct{})
		go func() {
			<-r.Context().Done()
			close(done)
		}()

		// Start streaming to client
		clientCh := server.AddClient(getClientIP(r), limit)
		handleStream(w, r, server, clientCh, done)
	})

	// Configure HTTP server
	server.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      mux,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start streaming in background
	go server.StartStreaming(*feedPath)

	// Start HTTP server
	go func() {
		log.Printf("Starting server on port %d", *port)
		if err := server.server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sig := <-sigChan
	log.Printf("Received signal: %v", sig)
	server.Shutdown()
}
