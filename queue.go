package main

import (
	"log"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"time"
)

var c chunk

func Start(storePath string) {
	files, err := ioutil.ReadDir(storePath)
	if err != nil {
		log.Printf("Directory read error %s", err)
		return
	}
	for {
		rand.Seed(time.Now().UnixNano())
		rand.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
		for _, file := range files {
			fName := file.Name()
			log.Printf("Loading file %s", fName)
			if filepath.Ext(fName) != ".mp3" {
				continue
			}
			f, err := os.Open(path.Join(storePath, fName))
			defer f.Close()
			if err != nil {
				log.Printf("File read error: %s", err)
				return
			}

			go c.Load(f)
			<-done
		}
	}
}
