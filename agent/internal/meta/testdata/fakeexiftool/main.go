// Command fakeexiftool speaks just enough of ExifTool's stay-open protocol for
// runner tests. It is built on demand and never shipped.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	binary := false
	tag := ""
	for _, arg := range os.Args[1:] {
		if arg == "-b" {
			binary = true
		}
		if strings.HasPrefix(arg, "-JpgFromRaw") {
			tag = "JpgFromRaw"
		}
	}
	if binary {
		if tag == "JpgFromRaw" {
			_, _ = os.Stdout.Write([]byte{0xFF, 0xD8, 0x01, 0x02, 0xFF, 0xD9})
		}
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	last := ""
	for scanner.Scan() {
		line := scanner.Text()
		if line != "-execute" {
			last = line
			continue
		}
		if last == "hang" {
			time.Sleep(10 * time.Second)
		}
		fmt.Println(`[{"EXIF:Make":"Canon","EXIF:Model":"Canon EOS R5","File:ImageWidth":6000,"File:ImageHeight":4000}]`)
		fmt.Println("{ready}")
	}
}
