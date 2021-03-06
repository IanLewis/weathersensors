// Command aggre_mod is an aggregator for device data.
// It receives data via the Particle pub/sub API
// and writes it to fluentd on the
// "aggre_mod.sensordata" channel.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/donovanhide/eventsource"
	"github.com/fluent/fluent-logger-golang/fluent"
	"github.com/najeira/ltsv"
)

//go:generate go run scripts/gen.go

const PARTICLE_API_URL = "https://api.particle.io/v1/devices/events/weatherdata"

// stringDefaults takes a default value and a list of string values and returns the first
// non-empty value. If all values are empty or there are no values present
// the default string value is returned.
func stringDefaults(def string, val ...string) string {
	for i := range val {
		if val[i] != "" {
			return val[i]
		}
	}
	return def
}

// intDefaults takes a default int value and a list of string values and returns the first
// non-empty value that can be converted to an integer. If all values are empty
// or there are no values present the default int value is returned.
func intDefaults(def int, val ...string) int {
	for i := range val {
		if val[i] != "" {
			intVal, err := strconv.ParseInt(val[i], 10, 32)
			if err == nil {
				return int(intVal)
			}
		}
	}
	return def
}

// boolDefaults takes a default bool value and a list of string values and returns the first
// non-empty value converted to a boolean. If all values are empty or there are
// no values present the default int value is returned.
func boolDefaults(def bool, val ...string) bool {
	for i := range val {
		if val[i] != "" {
			return strings.ToLower(val[i]) == "true"
		}
	}
	return def
}

var (
	addr = flag.String("host", stringDefaults(":8080", os.Getenv("ADDRESS")), "The web server address.")

	fluentdHost      = flag.String("fluentd-host", stringDefaults("localhost", os.Getenv("FLUENTD_HOST")), "The fluentd host.")
	fluentdPort      = flag.Int("fluentd-port", intDefaults(24224, os.Getenv("FLUENTD_PORT")), "The fluentd port.")
	fluentdRetryWait = flag.Int("fluentd-retry", intDefaults(500, os.Getenv("FLUENTD_RETRY_WAIT")), "Amount of time is milliseconds to wait between retries.")

	accessTokenPath   = flag.String("access-token-path", stringDefaults("", os.Getenv("ACCESS_TOKEN_PATH")), "The path to a file containing the Particle API access token.")
	particleRetryWait = flag.Int("particle-retry", intDefaults(500, os.Getenv("PARTICLE_RETRY_WAIT")), "Amount of time is milliseconds to wait between retries.")

	deviceTimeout = flag.Int("deviceTimeout", intDefaults(300, os.Getenv("DEVICE_TIMEOUT")), "The device timeout in seconds.")

	version = flag.Bool("version", false, "Print the version and exit.")
)

var (
	fluentdConnected     = false
	particleAPIConnected = false
)

type Device struct {
	Id            string   `json:"id"`
	Temp          *float64 `json:"current_temp"`
	Humidity      *float64 `json:"current_humidity"`
	Pressure      *float64 `json:"current_pressure"`
	WindSpeed     *float64 `json:"current_windspeed"`
	WindDirection *float64 `json:"current_winddirection"`
	Rainfall      *float64 `json:"current_rainfall"`
	LastSeen      int64    `json:"last_seen"`
	Active        bool     `json:"active"`
}

// A list of currently known devices
var Devices = []Device{}
var DeviceChan = make(chan map[string]interface{}, 100)

// Gets the access token for the Particle API by reading it from
// the access token secret file.
func getAccessToken() string {
	f, err := os.Open(*accessTokenPath)
	if err != nil {
		log.Fatal("Could not open access token file: ", err)
	}
	defer f.Close()
	b, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal("Could not open access token file: ", err)
	}
	return string(b)
}

// Data message from the Particle API
type Message struct {
	Id          string `json:"coreid"`
	Data        string `json:"data"`
	Ttl         string `json:"ttl"`
	PublishedAt string `json:"published_at"`
}

// addFloatValue takes a string containing a float value and adds it to a JSON object.
func addFloatValue(name string, jsonValue map[string]interface{}, data map[string]string) {
	if data[name] != "" {
		if val, err := strconv.ParseFloat(data[name], 64); err == nil {
			jsonValue[name] = val
		} else {
			log.Printf("Error parsing %s data: %v", name, err)
		}
	}
}

// connectToFluentd continuously tries to connect to Fluentd.
func connectToFluentd() *fluent.Fluent {
	var err error
	var logger *fluent.Fluent

	// Continuously try to connect to Fluentd.
	backoff := time.Duration(*fluentdRetryWait) * time.Millisecond
	for {
		log.Printf("Connecting to Fluentd (%s:%d)...", *fluentdHost, *fluentdPort)
		logger, err = fluent.New(fluent.Config{
			FluentHost: *fluentdHost,
			FluentPort: *fluentdPort,
			// Once we have a connection, the library will reconnect automatically
			// if the connection is lost. However, it panics if it fails to connect
			// more than MaxRetry times. To avoid panics crashing the server, retry
			// many times before panicking.
			MaxRetry:  240,
			RetryWait: *fluentdRetryWait,
		})
		if err != nil {
			log.Printf("Could not connect to Fluentd: %v", err)
			time.Sleep(backoff)
			backoff *= 2
		} else {
			log.Printf("Connected to Fluentd (%s:%d)...", *fluentdHost, *fluentdPort)
			return logger
		}
	}
}

// connectToParticle continuously tries to connect to the Particle API.
func connectToParticle(accessToken string) *eventsource.Stream {
	backoff := time.Duration(*particleRetryWait) * time.Millisecond

	for {
		req, err := http.NewRequest("GET", PARTICLE_API_URL, nil)
		if err != nil {
			log.Fatalf("Could not create request: %v", err)
			time.Sleep(time.Duration(*particleRetryWait) * time.Millisecond)
			continue
		}

		req.Header.Set("Authorization", "Bearer "+accessToken)
		log.Printf("Connecting to Particle API...")
		stream, err := eventsource.SubscribeWithRequest("", req)
		if err != nil {
			log.Printf("Could not subscribe to Particle API stream: %v", err)
			time.Sleep(backoff)
			backoff *= 2
		} else {
			log.Printf("Connected to Particle API...")
			return stream
		}
	}
}

// processData processes data incoming from devices and sends them over to Fluentd
func processData(accessToken string) {
	var err error

	// Connect to Fluentd
	logger := connectToFluentd()
	fluentdConnected = true

	// The stream object reconnects with exponential backoff.
	stream := connectToParticle(accessToken)
	particleAPIConnected = true

	// Now actually process events.
	for {
		// Block on the data/error channels.
		select {
		case event := <-stream.Events:
			// Unmarshall the JSON data from the Particle API.
			//////////////////////////////////////////////////////////////////
			var m Message
			jsonData := event.Data()
			// The particle API often sends newlines.
			// Perhaps as a keep-alive mechanism.
			if jsonData == "" {
				continue
			}
			err = json.Unmarshal([]byte(jsonData), &m)
			if err != nil {
				log.Printf("Could not parse message data: %v", err)
				continue
			}

			// Read LTSV data from the device into map[string]string
			//////////////////////////////////////////////////////////////////
			reader := ltsv.NewReader(bytes.NewBufferString(m.Data))
			records, err := reader.ReadAll()
			if err != nil || len(records) != 1 {
				log.Printf("Error reading LTSV data: %v", err)
				continue
			}

			data := records[0]
			log.Printf("Got data: %v", data)

			// Put the data into jsonValue and send to Fluentd
			//////////////////////////////////////////////////////////////////
			jsonValue := make(map[string]interface{})

			jsonValue["deviceid"] = m.Id

			timestamp, err := strconv.ParseInt(data["timestamp"], 10, 64)
			if err != nil {
				log.Printf("Error reading timestamp: %v", err)
				continue
			}

			jsonValue["timestamp"] = timestamp
			addFloatValue("temp", jsonValue, data)
			addFloatValue("humidity", jsonValue, data)
			addFloatValue("pressure", jsonValue, data)
			addFloatValue("windspeed", jsonValue, data)
			addFloatValue("winddirection", jsonValue, data)
			addFloatValue("rainfall", jsonValue, data)

			DeviceChan <- jsonValue

			// Send data directly to Fluentd
			if err = logger.Post("aggre_mod.sensordata", jsonValue); err != nil {
				log.Printf("Could not send data from %s to Fluentd: %v", m.Id, err)
			} else {
				log.Printf("Data processed (%s): %s", m.Id, data)
			}
		case err := <-stream.Errors:
			log.Printf("Stream error: %v", err)
		}
	}
}

// updateDevices updates devices periodically with the latest data.
func updateDevices() {
	// TODO: Need to split up this logic.
	for {
		select {
		case deviceInfo := <-DeviceChan:
			log.Println("Updating device:", deviceInfo["deviceid"])
			updateDevice(deviceInfo)
		default:
			for i, d := range Devices {
				active := time.Now().Unix()-d.LastSeen < int64(*deviceTimeout)
				if d.Active && !active {
					// Log a warning if a device is no longer active.
					log.Println("Device no longer active:", d.Id)
				}
				// Update the active flag.
				Devices[i].Active = active
			}
			// Throttle the loop if there is no data.
			time.Sleep(1 * time.Second)
		}
	}
}

// Updates a device with it's current status.
func updateDevice(jsonValue map[string]interface{}) {
	lastSeen := jsonValue["timestamp"].(int64)
	active := time.Now().Unix()-lastSeen < int64(*deviceTimeout)

	for _, d := range Devices {
		if d.Id == jsonValue["deviceid"].(string) {
			// Update known device
			if temp, ok := jsonValue["temp"]; ok {
				tempFloat := temp.(float64)
				d.Temp = &tempFloat
			} else {
				d.Temp = nil
			}
			if humidity, ok := jsonValue["humidity"]; ok {
				hFloat := humidity.(float64)
				d.Humidity = &hFloat
			} else {
				d.Humidity = nil
			}
			if pressure, ok := jsonValue["pressure"]; ok {
				pFloat := pressure.(float64)
				d.Pressure = &pFloat
			} else {
				d.Pressure = nil
			}
			if windspeed, ok := jsonValue["windspeed"]; ok {
				wsFloat := windspeed.(float64)
				d.WindSpeed = &wsFloat
			} else {
				d.WindSpeed = nil
			}
			if winddirection, ok := jsonValue["winddirection"]; ok {
				wdFloat := winddirection.(float64)
				d.WindDirection = &wdFloat
			} else {
				d.WindDirection = nil
			}
			if rainfall, ok := jsonValue["rainfall"]; ok {
				rFloat := rainfall.(float64)
				d.Rainfall = &rFloat
			} else {
				d.Rainfall = nil
			}
			d.LastSeen = lastSeen
			if d.Active && !active {
				// Log a warning if a device is no longer active.
				log.Println("Device no longer active:", d.Id)
			}
			d.Active = active
			return
		}
	}

	// New device
	d := Device{
		Id:       jsonValue["deviceid"].(string),
		LastSeen: lastSeen,
		Active:   active,
	}

	if temp, ok := jsonValue["temp"]; ok {
		tempFloat := temp.(float64)
		d.Temp = &tempFloat
	} else {
		d.Temp = nil
	}
	if humidity, ok := jsonValue["humidity"]; ok {
		hFloat := humidity.(float64)
		d.Humidity = &hFloat
	} else {
		d.Humidity = nil
	}
	if pressure, ok := jsonValue["pressure"]; ok {
		pFloat := pressure.(float64)
		d.Pressure = &pFloat
	} else {
		d.Pressure = nil
	}
	if windspeed, ok := jsonValue["windspeed"]; ok {
		wsFloat := windspeed.(float64)
		d.WindSpeed = &wsFloat
	} else {
		d.WindSpeed = nil
	}
	if winddirection, ok := jsonValue["winddirection"]; ok {
		wdFloat := winddirection.(float64)
		d.WindDirection = &wdFloat
	} else {
		d.WindDirection = nil
	}
	if rainfall, ok := jsonValue["rainfall"]; ok {
		rFloat := rainfall.(float64)
		d.Rainfall = &rFloat
	} else {
		d.Rainfall = nil
	}

	Devices = append(Devices, d)
}

// the logger as an io.Writer
type LogWriter struct{ *log.Logger }

func (w LogWriter) Write(b []byte) (int, error) {
	w.Printf("%s", b)
	return len(b), nil
}

// Returns the health status of the app.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	errorMsg := []string{}
	if !fluentdConnected {
		errorMsg = append(errorMsg, "fluentd: Not connected.")
	}
	if !particleAPIConnected {
		errorMsg = append(errorMsg, "particle: Not connected.")
	}

	if fluentdConnected && particleAPIConnected {
		fmt.Fprintf(w, "OK")
	} else {
		http.Error(w, strings.Join(errorMsg, "\n"), http.StatusInternalServerError)
	}
}

// Prints the server verison
func versionHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, VERSION)
}

func devicesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	dec := json.NewEncoder(w)
	dec.Encode(Devices)
}

func main() {
	flag.Parse()

	if *version {
		fmt.Println(VERSION)
		return
	}

	// Get the API access token
	accessToken := getAccessToken()

	// Process data in the background.
	go processData(accessToken)

	// Update device data periodically.
	go updateDevices()

	// Start the web server
	http.HandleFunc("/_status/healthz", healthHandler)
	http.HandleFunc("/_status/version", versionHandler)
	http.HandleFunc("/api/devices", devicesHandler)

	log.Printf("Listening on %s...", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
