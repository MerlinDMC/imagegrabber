package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"time"
)

type FlickrResult struct {
	Photos []FlickrResultPhoto `json:"photos"`
}

type FlickrResultPhoto struct {
	Name        string                           `json:"name"`
	Description string                           `json:"description"`
	Sizes       map[string]FlickrResultPhotoSize `json:"sizes"`
}

type FlickrResultPhotoSize struct {
	Label    string `json:"label"`
	Filename string `json:"file"`
	Url      string `json:"url"`
}

const (
	// flickr search url for a json api request that needs a query clause and a page number
	flickrSearchUrl string = "http://www.flickr.com/search?data=1&mt=photos&cm=&m=&l=&w=&hd=&d=&append=0&s=&q=%s&page=%d"
)

var (
	targetDirectory string
	sizeWanted      string
	maxPages        int
	concurrency     int
)

func init() {
	flag.StringVar(&targetDirectory, "outdir", os.TempDir(), "output directory for saved images")
	flag.StringVar(&sizeWanted, "size", "o", "size of the picture to grab [o=original, sq=square, q=large square, t=thumbnail, s=small, m=medium]")
	flag.IntVar(&maxPages, "max_pages", 3, "maximum number of pages to grab")
	flag.IntVar(&concurrency, "concurrency", 4, "maximum number parallel fetches")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "%s <flags> <search values>\n\n", path.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "default flags:\n")
		flag.PrintDefaults()
	}
}

func main() {
	if !flag.Parsed() {
		flag.Parse()
	}

	log.Printf("grabbing images for search clause: %s", strings.Join(flag.Args(), " "))
	defer log.Println("finished.")

	finishing := false
	wg := sync.WaitGroup{}
	stop := make(chan struct{})
	signals := make(chan os.Signal, 1)
	grab_pictures := make(chan FlickrResultPhotoSize, 100*concurrency)

	signal.Notify(signals, os.Interrupt, os.Kill)

	go func() {
		for {
			select {
			case <-signals:
				log.Printf("caught kill signal ... sending stop signal")

				finishing = true
				close(stop)

				signal.Stop(signals)

				return
			default:
				time.Sleep(1)
			}
		}
	}()

	log.Printf("starting grabbing goroutines")
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go grabPictures(grab_pictures, stop, &wg)
	}

	log.Printf("start grabbing picture uris")
	fetchFlickrData(strings.Join(flag.Args(), " "), grab_pictures, stop)

	for !finishing {
		if len(grab_pictures) == 0 {
			close(stop)

			break
		}

		time.Sleep(1)
	}

	log.Printf("waiting for grabbers to be done")

	wg.Wait()
}

func fetchFlickrData(searchClause string, pictures chan FlickrResultPhotoSize, stop chan struct{}) {
	createRequest := func(rawurl string) (*http.Request, error) {
		req, err := http.NewRequest("GET", rawurl, nil)
		if err == nil {
			req.Header.Set("Accept", "application/json")
		}

		return req, err
	}

	query := url.QueryEscape(searchClause)

	client := &http.Client{}

	var req *http.Request
	var res *http.Response
	var err error

	for page := 1; page <= maxPages; page++ {
		select {
		case <-stop:
			log.Printf("stopping flickr spider")
			return
		default:
		}

		req, err = createRequest(fmt.Sprintf(flickrSearchUrl, query, page))
		if err != nil {
			log.Fatalf("error creating request: %s", err)
		}

		res, err = client.Do(req)
		if err != nil {
			log.Fatalf("error while getting response: %s", err)
		}

		if res.Header.Get("Content-Type") != "application/json" {
			log.Printf("result is not application/json -> assuming end of list")

			return
		}

		var result FlickrResult
		var body []byte

		body, err = ioutil.ReadAll(res.Body)
		if err != nil {
			log.Fatalf("error while reading response body: %s", err)
		}

		res.Body.Close()

		err = json.Unmarshal(body, &result)
		if err != nil {
			log.Printf("can't parse json structure -> assuming end of list")
			log.Printf("Error: %s", err)
			break
		}

		for _, photo := range result.Photos {
			if sized, ok := photo.Sizes[sizeWanted]; ok {
				// we have the requested size -> put it on the channel for post processing
				select {
				case <-stop:
					log.Printf("stopping flickr spider")
					return
				case pictures <- sized:
					log.Printf("pushed picture: %s", sized.Filename)
				default:
				}
			}
		}
	}
}

func grabPictures(queue chan FlickrResultPhotoSize, stop chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
	nextPicture:
		select {
		case sized := <-queue:
			var retries int

			for retries = 0; retries < 5; retries++ {
				if res, err := http.Get(sized.Url); err == nil {
					log.Printf("fetching: %s (%d bytes, try #%d)", sized.Filename, res.ContentLength, retries)

					if fo, err := os.Create(fmt.Sprintf("%s/%s", targetDirectory, sized.Filename)); err == nil {
						io.Copy(fo, res.Body)

						fo.Close()
						res.Body.Close()

						break nextPicture
					}
				} else {
					time.Sleep(time.Duration(2 * (retries + 1)))
				}
			}

			log.Printf("could not fetch after %d retries -> assuming hard error", retries)
		case <-stop:
			log.Printf("stopping picture grabber")
			return
		default:
		}
	}
}
