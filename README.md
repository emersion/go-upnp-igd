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
	devices := igd.Discover(10 * time.Second)
	for _, d := range devices {
		log.Println(d)
	}
}
```

## License

MPL 2.0
