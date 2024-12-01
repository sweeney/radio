package main

import (
	"encoding/xml"
	"net/http"
	"sync"
	"time"
)

// MP3 constants
const (
	frameSyncBits = 0xFFE0
	minFrameSize  = 4
)

// Bitrate lookup table (kbps) - [version][layer][index]
var mp3Bitrates = [4][4][16]int{
	{ // Version 2.5
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},                       // Reserved
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},      // Layer 3
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},      // Layer 2
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0}, // Layer 1
	},
	{ // Reserved
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	},
	{ // Version 2
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},                       // Reserved
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},      // Layer 3
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},      // Layer 2
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0}, // Layer 1
	},
	{ // Version 1
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},                          // Reserved
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},     // Layer 3
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},    // Layer 2
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0}, // Layer 1
	},
}

// Sample rate lookup table (Hz) - [version][index]
var mp3SampleRates = [4][4]int{
	{11025, 12000, 8000, 0},  // Version 2.5
	{0, 0, 0, 0},             // Reserved
	{22050, 24000, 16000, 0}, // Version 2
	{44100, 48000, 32000, 0}, // Version 1
}

// MP3Header represents parsed MP3 frame header information
type MP3Header struct {
	frameSize  int
	bitrate    int
	sampleRate int
	padding    bool
}

// MP3Frame represents a single MP3 frame
type MP3Frame struct {
	data []byte
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
