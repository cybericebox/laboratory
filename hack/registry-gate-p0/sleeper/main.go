// Command sleeper is the only file of the image the P0 fake gate serves: it keeps the container running.
package main

import "time"

func main() {
	for {
		time.Sleep(time.Hour)
	}
}
