package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gookit/color"
	"github.com/hpcloud/tail"
	"golang.org/x/sys/unix"
)

// Source path specification:
//    env var substitution
//    relative to nexus.json
//    relative to pwd
//    file globbing e.g. "/var/log/*.log"

// isolate to single log, select which logs to show
// filtering (interrupts and prompts for filter)
// groups (e.g. crons, ...) disable/enable
// print warning for inaccessible files (permission) and missing files
// highlighting certain words (e.g. error/fatal/warning)
// allow running commands and logging output (e.g. `dmesg`, `journalctl -fu foo.service`)
// limit number of initial lines shown from each source
// option: prepend timestamps to each message
// layout options
//    long lines: truncate, split
//    single line logging (source + message combined), multi-line logging (source and message on separate lines)
//    multi-line: don't print header unless source has changed or enough time has elapsed since last log message
// commands
//    pause/resume logging
//    filtering
//    highlighting
//    list sources with colors/names/paths
// option to suppress source names, acts as tail -f ... across a bunch of files
// ability to specify highlight at runtime (highlight bg yellow)
// command-line options for the above
// ability to show logs by name in config (e.g. nexus --source php/errors --source laravel)
// bug: ctrl-c while running on FormulateDevServer2 while initial tail is happening
// bug: fail when running via ssh (ssh vm nexus), problems with detecting terminal size

type source struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	Background *[3]int `json:"bg"`
	Foreground *[3]int `json:"fg"`
	Truncate   bool    `json:"truncate"`
}

type config struct {
	Sources []*source `json:"sources"`
}

func loadConfig(filename string) (*config, error) {
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config *config
	if err = json.Unmarshal(content, &config); err != nil {
		return nil, err
	}

	return config, nil
}

// See https://stackoverflow.com/a/56678483.
func perceivedLightness(r, g, b int) float64 {
	nr := float64(r) / 255
	ng := float64(g) / 255
	nb := float64(b) / 255

	linearize := func(color float64) float64 {
		if color <= 0.04045 {
			return color / 12.92
		}

		return math.Pow((color+0.055)/1.055, 2.4)
	}

	// Luminance.
	y := 0.2126*linearize(nr) + 0.7152*linearize(ng) + 0.0722*linearize(nb)

	if y <= 0.008856 {
		return y * 903.3
	} else {
		return math.Pow(y, 1.0/3)*116 - 16
	}
}

func getTerminalSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		panic(err)
	}

	return int(ws.Col), int(ws.Row)
}

type record struct {
	Source *source
	Line   *tail.Line
}

func writer(records <-chan *record, stop <-chan bool) {
	width, height := getTerminalSize()

	var lastSource *source
	streak := 0

	primaries := make(map[*source]color.RGBColor)
	styles := make(map[*source]color.RGBStyle)

	getStyle := func(src *source) (color.RGBColor, color.RGBStyle) {
		var primary color.RGBColor
		var style color.RGBStyle
		var ok bool

		if primary, ok = primaries[src]; !ok {
			var r, g, b int
			if src.Background == nil {
				hash := sha256.Sum256([]byte(src.Path))
				r, g, b = int(hash[0]), int(hash[1]), int(hash[2])
			} else {
				r, g, b = src.Background[0], src.Background[1], src.Background[2]
			}

			var fg, bg color.RGBColor

			primary = color.RGB(uint8(r), uint8(g), uint8(b))
			primaries[src] = primary
			bg = primary

			if src.Foreground == nil {
				if perceivedLightness(r, g, b) >= 50 {
					fg = color.RGB(0, 0, 0)
				} else {
					fg = color.RGB(255, 255, 255)
				}
			} else {
				r, g, b = src.Foreground[0], src.Foreground[1], src.Foreground[2]
				fg = color.RGB(uint8(r), uint8(g), uint8(b))
			}

			style = *color.NewRGBStyle(fg, bg)
			styles[src] = style
		} else {
			style = styles[src]
		}

		return primary, style
	}

	for {
		select {
		case <-stop:
			return

		case record := <-records:
			if record.Source != lastSource {
				primary, style := getStyle(record.Source)
				style.Printf(" %s ", record.Source.Name)
				primary.Printf(" %s", record.Source.Path)
				fmt.Println()
				streak = 1
			} else {
				streak++
				if streak == height {
					primary, style := getStyle(record.Source)
					style.Printf(" %s (cont) ", record.Source.Name)
					primary.Printf(" %s", record.Source.Path)
					fmt.Println()
					streak = 1
				}
			}

			if record.Source.Truncate && len(record.Line.Text) >= width {
				fmt.Println(record.Line.Text[0 : width-1])
			} else {
				fmt.Println(record.Line.Text)
			}

			lastSource = record.Source
		}
	}
}

func main() {
	config, err := loadConfig("nexus.json")
	if err != nil {
		log.Fatal(err)
	}

	var wg sync.WaitGroup

	files := make([]*tail.Tail, 0)
	records := make(chan *record, 1024)

	for _, src := range config.Sources {
		file, err := tail.TailFile(src.Path, tail.Config{Follow: true})
		if err != nil {
			fmt.Println(">>>>>>>>>>>>>>>>>>>>> error ", err)
			continue
		}
		files = append(files, file)

		wg.Add(1)
		go func(src *source, file *tail.Tail) {
			for line := range file.Lines {
				records <- &record{src, line}
			}

			fmt.Printf(">>>>>>>>>>>>>>>>>>>>> stop for %s %v\n ", src.Path, file.Err())
			wg.Done()
		}(src, file)
	}

	stopWriter := make(chan bool, 1)
	go writer(records, stopWriter)

	// Wait for interrupt.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	<-interrupt

	stopWriter <- true

	for _, file := range files {
		file.Stop()
		file.Cleanup()
	}

	wg.Wait()
}
