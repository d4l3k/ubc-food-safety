package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/jasonwinn/geocoder"
)

const (
	restaurantsURL = "https://inspections.vcha.ca/FoodPremises/Table?SortMode=FacilityName&page=1&PageSize=100000"
	dbFile         = "restaurants.json"

	borderLng = -123.227883
)

type latLong struct {
	Lat, Long float64
}

type db struct {
	Restaurants []*restaurant

	GeocodeCache map[string]latLong
}

func makeDB() *db {
	return &db{
		GeocodeCache: map[string]latLong{},
	}
}

func (db *db) load() error {
	f, err := os.OpenFile(dbFile, os.O_RDONLY, 0755)
	if os.IsNotExist(err) {
		log.Println("Can't load DB; not exist")
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()

	return json.NewDecoder(f).Decode(db)
}

func (db *db) save() error {
	f, err := os.OpenFile(dbFile, os.O_CREATE|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	return encoder.Encode(db)
}

type inspection struct {
	Date                  string
	Number                string
	Reason                string
	NonCritical, Critical int
}

type restaurant struct {
	ID             string
	Name           string
	FacilityType   string
	Community      string
	SiteAddress    string
	PhoneNumber    string
	MoreDetailsURL string

	OutstandingNonCriticalInfractions, OutstandingCriticalInfractions int

	Inspections []inspection

	LatLong latLong

	InfractionsPastYear int
	InfractionsTotal    int
}

func resolveURL(base, rel string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	relURL, err := url.Parse(rel)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(relURL).String(), nil
}

func get(addr string) (*goquery.Document, error) {
	req, err := http.NewRequest("GET", addr, nil)
	if err != nil {
		return nil, err
	}
	req.AddCookie(&http.Cookie{
		Name:  "ASP.NET_SessionId",
		Value: "uiktkmxmg2fq3jw1pvwc4kgp",
	})
	log.Printf("Fetching: %s", addr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromResponse(resp)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func getRestaurants() ([]*restaurant, error) {
	doc, err := get(restaurantsURL)
	if err != nil {
		return nil, err
	}

	var restaurants []*restaurant
	doc.Find("tr.hovereffect").Each(func(_ int, s *goquery.Selection) {
		var r restaurant
		r.Name = strings.TrimSpace(s.Find(".facilityName").Text())
		r.FacilityType = strings.TrimSpace(s.Find(".facilityType").Text())
		r.Community = strings.TrimSpace(s.Find(".community").Text())
		r.SiteAddress = strings.TrimSpace(s.Find(".siteAddress").Text())
		r.PhoneNumber = strings.TrimSpace(s.Find(".phoneNumber").Text())

		onClick := strings.TrimSpace(s.AttrOr("onclick", ""))
		url := strings.Split(onClick, "'")[1]
		r.ID = path.Base(url)
		r.MoreDetailsURL, err = resolveURL(restaurantsURL, url)
		if err != nil {
			log.Println(err)
		}

		restaurants = append(restaurants, &r)
	})
	return restaurants, nil
}

func (db *db) geocode(address string) (latLong, error) {
	if len(address) == 0 {
		return latLong{}, errors.New("address empty")
	}

	address = strings.Join(strings.Split(address, "\n"), ", ")
	cached, ok := db.GeocodeCache[address]
	if ok {
		return cached, nil
	}

	log.Printf("GEOCODE:\n%s", address)
	lat, lng, err := geocoder.Geocode(address)
	if err != nil {
		return latLong{}, err
	}

	cached = latLong{Lat: lat, Long: lng}
	db.GeocodeCache[address] = cached

	return cached, nil
}

const vancouverWestside = "Vancouver - Westside"

func (db *db) geocodeRestaurants() error {
	log.Printf("Geocoding %d restaurants...", len(db.Restaurants))
	for i, r := range db.Restaurants {
		if r.Community != vancouverWestside {
			continue
		}
		log.Printf("Coding %d", i)
		latLong, err := db.geocode(r.SiteAddress)
		if err != nil {
			return err
		}
		r.LatLong = latLong
	}
	return nil
}

func (db *db) getUBCRestaurants() []*restaurant {
	var rs []*restaurant
	for _, r := range db.Restaurants {
		if r.LatLong.Long < borderLng {
			rs = append(rs, r)
		}
	}
	return rs
}

func computeInfractionsPastYear(rs []*restaurant) error {
	yearAgo := time.Now().AddDate(-1, 0, 0)
	for _, r := range rs {
		count := 0
		total := 0
		for _, i := range r.Inspections {
			date, err := time.Parse("02-Jan-2006", i.Date)
			if err != nil {
				return err
			}
			if date.After(yearAgo) {
				count += i.Critical + i.NonCritical
			}
			total += i.Critical + i.NonCritical
		}
		r.InfractionsPastYear = count
		r.InfractionsTotal = total
	}
	return nil
}

func printRestaurants(rs []*restaurant) {
	fmt.Println("|Name|Infractions (Past Year)|Infractions (Total)|Outstanding Critical Infractions|Outstanding Non-CriticalInfractions||")
	fmt.Println("|---|---|---|---|---|---|")
	for _, r := range rs {
		if len(r.Inspections) == 0 {
			continue
		}

		fmt.Printf("|%s|%d|%d|%d|%d|[Details](%s)|\n", r.Name, r.InfractionsPastYear, r.InfractionsTotal, r.OutstandingCriticalInfractions, r.OutstandingNonCriticalInfractions, r.MoreDetailsURL)
	}
}

const workers = 16

func fetchDetail(r *restaurant) error {
	doc, err := get(r.MoreDetailsURL)
	if err != nil {
		return err
	}
	doc.Find("tr.nozebrastripes").Each(func(_ int, s *goquery.Selection) {
		label := strings.TrimSpace(s.Find(".display-label").Text())
		field := strings.TrimSpace(s.Find(".display-field").Text())
		if label == "Outstanding Non-Critical Infractions" {
			r.OutstandingNonCriticalInfractions, err = strconv.Atoi(field)
			if err != nil {
				log.Println(err)
			}
		} else if label == "Outstanding Critical Infractions" {
			r.OutstandingCriticalInfractions, err = strconv.Atoi(field)
			if err != nil {
				log.Println(err)
			}
		}
	})

	var inspections []inspection
	doc.Find("tr.hovereffect").Each(func(_ int, s *goquery.Selection) {
		var i inspection
		i.Date = strings.TrimSpace(s.Find(".inspectionDate").Text())
		i.Number = strings.TrimSpace(s.Find(".inspectionNumber").Text())
		i.Reason = strings.TrimSpace(s.Find(".inspectionType").Text())
		i.Critical, err = strconv.Atoi(strings.TrimSpace(s.Find(".criticalInfractionsCount").Text()))
		if err != nil {
			log.Println(err)
		}
		i.NonCritical, err = strconv.Atoi(strings.TrimSpace(s.Find(".nonCriticalInfractionsCount").Text()))
		if err != nil {
			log.Println(err)
		}
		inspections = append(inspections, i)
	})
	r.Inspections = inspections

	return nil
}

func fetchDetails(rs []*restaurant) {
	rsChan := make(chan *restaurant, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for r := range rsChan {
				if err := fetchDetail(r); err != nil {
					log.Println(err)
					return
				}
			}
		}()
	}
	for _, r := range rs {
		if !(len(r.Inspections) == 0 || *refetch) {
			continue
		}
		rsChan <- r
	}
	close(rsChan)
	wg.Wait()
}

var refetch = flag.Bool("refetch", false, "whether to refetch all restaurants")

func generateRestaurantsList() error {
	db := makeDB()
	if err := db.load(); err != nil {
		return err
	}
	defer func() {
		if err := db.save(); err != nil {
			log.Println(err)
		}
	}()

	if len(db.Restaurants) == 0 || *refetch {
		restaurants, err := getRestaurants()
		if err != nil {
			return err
		}
		db.Restaurants = restaurants
	}
	if err := db.geocodeRestaurants(); err != nil {
		return err
	}
	ubc := db.getUBCRestaurants()
	// Uncomment to fetch all details. Last time I did this I hit them too hard
	// and they blocked me. :/
	//fetchDetails(db.Restaurants)
	fetchDetails(ubc)
	if err := computeInfractionsPastYear(db.Restaurants); err != nil {
		return err
	}

	sort.Slice(ubc, func(i, j int) bool {
		return ubc[i].InfractionsPastYear < ubc[j].InfractionsPastYear
	})
	printRestaurants(ubc)

	return nil
}

func main() {
	flag.Parse()
	geocoder.SetAPIKey("AYrMZCLVncowATRyqAc10zotuHotsH1r")

	if err := generateRestaurantsList(); err != nil {
		log.Fatal(err)
	}
}
