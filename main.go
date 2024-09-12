package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const MQTT_TOPIC_PREFIX = "zigbee2mqtt/"

var (
	// matches whole line comments in config file
	CONFIG_COMMENTS_RE = regexp.MustCompile(`(?m)^\s*//.*$`)

	// for MQTT server URI validation
	SERVER_URL_RE = regexp.MustCompile(`^[a-z]+://.*:[0-9]{1,5}$`)
)

// Program config, directly filled by json.Unmarshal
type config struct {
	// MQTT server & credentials
	Server, Username, Password string

	Location [2]float64 // lat, long

	OffDelay       string
	Sensor, Switch string
}

type device struct {
	id          string // internal device ID
	topic       string // MQTT topic
	stateAttr   string // state attribute
	state       any    // current state
	lastUpdated time.Time
}

func (d *device) DecodePayload(msg mqtt.Message) (payload map[string]any, changed bool, err error) {
	payload, err = decodePayload(msg)
	if err != nil {
		return payload, false, fmt.Errorf("unable to parse MQTT payload: %v", err)
	}

	changed = false

	if d.stateAttr != "" {
		attr, ok := payload[d.stateAttr]
		if !ok {
			return payload, false, fmt.Errorf("state attr %q not found for %q", d.stateAttr, d.topic)
		}

		// check and toggle state
		if attr != d.state && reflect.TypeOf(attr) == reflect.TypeOf(d.state) {
			d.state = attr
			changed = true
		}
	}

	return payload, changed, nil
}

type regelwerk struct {
	mu     sync.Mutex
	client mqtt.Client

	lat, lng                  float64
	currDate, sunrise, sunset time.Time

	offDelay           time.Duration
	sensorId, switchId string

	// timers
	timers   map[string]*timer
	timersMu sync.Mutex

	// devices
	devices     map[string]*device
	devicesById map[string]*device
}

func (r *regelwerk) AddDevice(d *device) {
	r.devices[d.topic] = d
	r.devicesById[d.id] = d
}

func (r *regelwerk) LookupDevice(id string) *device {
	return r.devicesById[id]
}

// timers management

type timer struct {
	t, expT *time.Timer
	fired   atomic.Uint32
}

func (r *regelwerk) mkTimerFunc(name string, expired bool, tm *timer) func() {
	return func() {
		// guard against timeout & expiry firing twice
		if tm.fired.CompareAndSwap(0, 1) {
			if *debugMode {
				ev := "fired"
				if expired {
					ev = "expired"
				}
				log.Printf("timer %q %s", name, ev)
			}

			r.Lock()
			r.handleTimer(name, expired)
			r.Unlock()

			r.timersMu.Lock()
			defer r.timersMu.Unlock()

			if r.timers[name] == tm {
				delete(r.timers, name)
			}
		}
	}
}

func (r *regelwerk) AddTimer(name string) *timer {
	tm := &timer{}
	t := time.AfterFunc(time.Hour, r.mkTimerFunc(name, false, tm))
	t.Stop()
	tm.t = t

	r.timersMu.Lock()
	defer r.timersMu.Unlock()

	// do not overwrite existing timers
	if _, exists := r.timers[name]; !exists {
		r.timers[name] = tm
		return tm
	}
	return nil
}

func (r *regelwerk) AddTimerWithExpiry(name string, expiry time.Duration) *timer {
	tm := r.AddTimer(name)
	// attach an expiry timer. this is unreferenced and there's no way to stop it
	if tm != nil {
		tm.expT = time.AfterFunc(expiry, r.mkTimerFunc(name, true, tm))
	}
	return tm
}

func (r *regelwerk) DestroyTimer(name string) bool {
	r.timersMu.Lock()
	defer r.timersMu.Unlock()

	if t := r.timers[name]; t != nil {
		t.t.Stop()
		if t.expT != nil {
			t.expT.Stop()
		}

		delete(r.timers, name)
		return true
	}

	return false
}

// Tries to (re)start timer if it exists
// Returns whether the timer was found, false if it wasn't
func (r *regelwerk) StartTimer(name string, dur time.Duration) bool {
	r.timersMu.Lock()
	defer r.timersMu.Unlock()

	t, found := r.timers[name]
	if !found {
		return false
	}

	t.t.Reset(dur)
	return true
}

// Stop a timer, if found
// Does not affect the expiry timer; that continues running
func (r *regelwerk) StopTimer(name string) *timer {
	r.timersMu.Lock()
	defer r.timersMu.Unlock()

	t, found := r.timers[name]
	if !found {
		return nil
	}

	t.t.Stop()
	return t
}

// Determines if it's dusk
// If the location is specified in the config file, lazily computes the sunset/sunrise time
// or else just use a 7-to-7 time as the default dusk.
func (r *regelwerk) NowIsDusk() bool {
	ts := time.Now()

	// default dusk/dawn logic, 7pm - 7am
	isDusk := ts.Hour() >= 19 || ts.Hour() < 7

	// see if we should compute sunset/sunrise times
	if r.lat != 0 && r.lng != 0 {
		// lock already held in handleDeviceEvent
		//r.Lock()

		if !isSameDay(r.currDate, ts) {
			// need to compute timings for today
			r.sunrise = calcTimeAtSunAngle(ts, true, 96, r.lat, r.lng)
			r.sunset = calcTimeAtSunAngle(ts, false, 96, r.lat, r.lng)
			r.currDate = ts

			log.Printf("computed timings for %s:\nsunrise: %s\nsunset:  %s",
				ts.Format("02 Jan 2006"),
				r.sunrise.Format(time.RFC1123),
				r.sunset.Format(time.RFC1123))
		}
		//r.Unlock()

		isDusk = ts.Before(r.sunrise) || ts.After(r.sunset)
	}

	return isDusk
}

func (r *regelwerk) Lock()   { r.mu.Lock() }
func (r *regelwerk) Unlock() { r.mu.Unlock() }

func (r *regelwerk) sendSwitchState(turnOn bool) {
	state := "OFF"
	if turnOn {
		state = "ON"
	}

	if *debugMode {
		log.Printf("turning switch %v now", state)
	}

	r.client.Publish(MQTT_TOPIC_PREFIX+r.switchId+"/set", 0, false,
		`{"state_right":"`+state+`"}`)
}

// Decodes the payload as a JSON map
func decodePayload(msg mqtt.Message) (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal(msg.Payload(), &m)
	return m, err
}

// Retrieves a string value from a map
// If key doesn't exist or an error, returns an empty string
func getMapValue(m map[string]any, key string) string {
	v, exists := m[key]
	if !exists {
		return ""
	}
	vs, _ := v.(string)
	return vs
}

// Checks if given Times are for the same day
func isSameDay(t1, t2 time.Time) bool {
	y1, m1, d1 := t1.Date()
	y2, m2, d2 := t2.Date()
	return y1 == y2 && m1 == m2 && d1 == d2 && t1.Location() == t2.Location()
}

func (r *regelwerk) handleMqtt(_ mqtt.Client, msg mqtt.Message) {
	// check for and strip away z2m prefix
	topic := strings.TrimPrefix(msg.Topic(), MQTT_TOPIC_PREFIX)
	if topic == msg.Topic() {
		return
	}

	// ignore bridge device, as well as set/get requests
	if strings.HasSuffix(topic, "/set") ||
		strings.HasSuffix(topic, "/get") ||
		strings.HasPrefix(topic, "bridge/") {
		return
	}

	if *debugMode {
		log.Printf("recv %q, payload %s", msg.Topic(), msg.Payload())
	}

	dev, found := r.devices[topic]
	if found {
		r.Lock()
		defer r.Unlock()

		payload, changed, err := dev.DecodePayload(msg)
		if err != nil {
			log.Printf("error parsing MQTT msg: %v", err)
		} else {
			// fire for arbitrary events
			r.handleDeviceEvent(dev, payload)

			// fire only on change events
			if changed {
				if *debugMode {
					log.Printf("dev %q (%q) state %q changed to %#v",
						dev.id, dev.topic, dev.stateAttr, dev.state)
				}
				r.handleDeviceChangedEvent(dev, payload)
			}
		}
	}
}

func parseConfig(fname string, cfg *config) error {
	cfgStr, err := os.ReadFile(fname)
	if err != nil {
		return err
	}

	// remove line comments, json.Unmarshal can't parse them
	cfgStr = CONFIG_COMMENTS_RE.ReplaceAllLiteral(cfgStr, []byte{})

	return json.Unmarshal(cfgStr, cfg)
}

var (
	debugMode  = flag.Bool("debug", false, "output debug messages")
	configFile = flag.String("config", "/etc/regelwerk.conf", "config file")
)

func main() {
	flag.Parse()

	// check if we are running under systemd, and if so, dont output timestamps
	if a, b := os.Getenv("INVOCATION_ID"), os.Getenv("JOURNAL_STREAM"); a != "" && b != "" {
		log.SetFlags(0)
	}

	cfg := config{}
	if err := parseConfig(*configFile, &cfg); err != nil {
		log.Fatalf("unable to parse config: %v", err)
	}

	//log.Printf("config %+v\n", cfg)

	// sanity check
	if cfg.Server == "" {
		log.Fatal("MQTT server not specified")
	} else if !SERVER_URL_RE.MatchString(cfg.Server) {
		log.Fatal("invalid MQTT server: needs to be in URL format with port")
	}

	offDelay := 15 * time.Second
	if cfg.OffDelay != "" {
		cfg.OffDelay = strings.ReplaceAll(cfg.OffDelay, " ", "")

		var err error
		offDelay, err = time.ParseDuration(cfg.OffDelay)
		if err != nil {
			log.Fatalf("invalid OffDelay: %s", err)
		} else if offDelay.Seconds() < 0 {
			log.Fatal("OffDelay cannot be negative")
		}
	}

	r := &regelwerk{
		offDelay: offDelay,
		sensorId: cfg.Sensor,
		switchId: cfg.Switch,

		lat: cfg.Location[0],
		lng: cfg.Location[1] * -1, // our code has inverted longitude

		timers:      make(map[string]*timer),
		devices:     make(map[string]*device),
		devicesById: make(map[string]*device),
	}

	// add devices
	r.AddDevice(&device{
		id:        "contact",
		topic:     r.sensorId,
		stateAttr: "contact",
		state:     true,
	})

	r.AddDevice(&device{
		id:        "switch",
		topic:     r.switchId,
		stateAttr: "state_right",
		state:     "OFF",
	})

	//mqtt.DEBUG = log.New(os.Stdout, "[MQTT]", 0)

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.Server).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetClientID("regelwerk").
		SetDialer(&net.Dialer{KeepAlive: -1}).
		SetKeepAlive(60 * time.Second).
		SetPingTimeout(2 * time.Second).
		SetConnectRetry(true)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		tok := c.Subscribe(MQTT_TOPIC_PREFIX+"#", 0, r.handleMqtt)
		if tok.Wait() && tok.Error() != nil {
			log.Fatal(tok.Error())
		}

		log.Printf("subscribed to MQTT topic")
	})

	r.client = mqtt.NewClient(opts)

	log.Printf("connecting to MQTT broker %v...", cfg.Server)
	if tok := r.client.Connect(); tok.Wait() && tok.Error() != nil {
		log.Printf("cannot connect to MQTT broker: %v\n", tok.Error())
	}

	log.Printf("waiting for MQTT events...")
	select {}
}
