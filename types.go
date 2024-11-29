package main

import (
	"encoding/xml"
	"net/http"
	"sync"
	"time"
)

// MP3Header represents parsed MP3 frame header information
type MP3Header struct {
	bitrate    int
	sampleRate int
	padding    int
	frameSize  int
}

// MP3Frame represents a single MP3 frame with its header data
type MP3Frame struct {
	data   []byte
	header MP3Header
}

// RSS Feed structures for BBC podcast format
type Rss struct {
	XMLName xml.Name `xml:"rss"`
	Channel Channel  `xml:"channel"`
}

type Channel struct {
	Title    string `xml:"title"`
	Episodes []Item `xml:"item"`
}

type Enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type Item struct {
	Title       string    `xml:"title"`
	Description string    `xml:"description"`
	PubDate     string    `xml:"pubDate"`
	Duration    string    `xml:"duration"`
	Enclosure   Enclosure `xml:"enclosure"`
}

// Episode represents a downloadable podcast episode
type Episode struct {
	item     Item
	data     []byte
	frames   []MP3Frame
	position int
}

// Client represents a connected streaming client
type Client struct {
	ch       chan struct{}
	address  string
	lastSeen time.Time
}

// EpisodeManager handles episode downloading and buffering
type EpisodeManager struct {
	episodes     []Episode
	currentIndex int
	mutex        sync.RWMutex
	downloading  bool
}

// StreamServer handles client connections and streaming
type StreamServer struct {
	episodeManager *EpisodeManager
	clients        map[chan struct{}]*Client
	addresses      map[string]int
	mutex          sync.RWMutex
	shutdown       chan struct{}
	server         *http.Server
	activeConns    sync.WaitGroup
}
