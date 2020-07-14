package main

import (
	"log"
	"time"
	"os"

	"github.com/emersion/go-upnp-igd"
)

func main() {
	igd.Logger = log.New(os.Stderr, "", log.LstdFlags)

	devices := make(chan igd.Device)
	go func() {
		for d := range devices {
			log.Println(d)
		}
	}()

	err := igd.Discover(devices, 30*time.Second)
	if err != nil {
		log.Fatal(err)
	}
}
