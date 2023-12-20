package main

import (
        "flag"
	"log"
	"net/http"
)

var addr = flag.String("addr", "0.0.0.0:3000", "http service address")
var tracks = flag.String("tracks", "./tracks", "path to mp3 storage")

func main() {

        flag.Parse()

        log.Printf("Serving directory %s on address %s", *tracks, *addr)

	go Start(*tracks)
	handler := NewHandler()
	log.Fatal(http.ListenAndServe(*addr, handler))
}
