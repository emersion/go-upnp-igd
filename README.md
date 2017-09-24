# go-upnp-igd

[![GoDoc](https://godoc.org/github.com/emersion/go-upnp-igd?status.svg)](https://godoc.org/github.com/emersion/go-upnp-igd)

Minimal Go UPnP InternetGatewayDevice library. Based on
[Syncthing's library](https://github.com/syncthing/syncthing/tree/e8ba6d477182a73ba417d0d69999a104d04cd912/lib/upnp).

## Usage

```go
package main

import (
	"log"
	"time"

	"github.com/emersion/go-upnp-igd"
)

func main() {
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

```

## License

MPL 2.0
