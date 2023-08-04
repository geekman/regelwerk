package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
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

type stateSession struct {
	t *time.Timer
}

type regelwerk struct {
	mu     sync.Mutex
	client mqtt.Client

	lat, lng                  float64
	currDate, sunrise, sunset time.Time

	offDelay           time.Duration
	sensorId, switchId string

	switchIsOn    bool
	sensorContact bool
	session       *stateSession
}

func (r *regelwerk) Lock()   { r.mu.Lock() }
func (r *regelwerk) Unlock() { r.mu.Unlock() }

func (r *regelwerk) turnOff() {
	r.Lock()

	if r.session != nil { // check again, just in case
		if r.session.t != nil {
			r.session.t.Stop() // just in case
			r.session.t = nil
		}

		// remove session entirely
		r.session = nil
	}

	r.Unlock()

	r.sendSwitchState(false)
}

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

	ts := time.Now()

	if *debugMode {
		log.Printf("recv %q, payload %s", msg.Topic(), msg.Payload())
	}

	switch topic {
	case r.sensorId:
		payload, err := decodePayload(msg)
		if err != nil {
			log.Printf("unable to parse MQTT payload: %v", err)
			return
		}

		contact, ok := payload["contact"].(bool)
		if !ok {
			log.Printf("invalid contact value in payload: %+v", payload)
			return
		}

		// default dusk/dawn logic, 7pm - 7am
		shouldTurnOn := ts.Hour() >= 19 || ts.Hour() < 7

		// see if we should compute sunset/sunrise times
		if r.lat != 0 && r.lng != 0 {
			r.Lock()
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
			r.Unlock()

			shouldTurnOn = ts.Before(r.sunrise) || ts.After(r.sunset)
		}

		r.Lock()

		if r.sensorContact == contact {
			// just a periodic status message, ignore it
		} else if !contact { // door opened
			if r.session != nil {
				log.Printf("paused session for triggered sensor")
				if r.session.t != nil {
					r.session.t.Stop()
					r.session.t = nil
				}
			}

			if shouldTurnOn && !r.switchIsOn && r.session == nil {
				log.Printf("starting session for triggered sensor")

				r.session = &stateSession{}

				// send after mutex unlock
				defer r.sendSwitchState(true)
			}
		} else {
			if r.session != nil && r.session.t == nil {
				log.Printf("starting delayed turn-off after %s", r.offDelay)
				r.session.t = time.AfterFunc(r.offDelay, r.turnOff)
			}
		}

		// update internal state for contact sensor
		r.sensorContact = contact

		r.Unlock()

	case r.switchId:
		payload, err := decodePayload(msg)
		if err != nil {
			log.Printf("unable to parse MQTT payload: %v", err)
			return
		}

		state := getMapValue(payload, "state_right")
		if state == "" {
			log.Printf("invalid/missing state_xxx value in payload: %+v", payload)
			return
		}

		// action is optional, only when it was explicit
		action := getMapValue(payload, "action")

		r.Lock()
		defer r.Unlock()

		if action == "single_right" {
			if *debugMode {
				log.Printf("switch actuated: %v", action)
			}

			if r.session != nil {
				log.Printf("manual override - discarding current session")

				if r.session.t != nil {
					r.session.t.Stop()
					r.session.t = nil
				}

				// also destroy session
				r.session = nil
			}
		}

		// update internal switch status
		r.switchIsOn = state == "ON"
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

		sensorContact: true, // default state: closed contact

		lat: cfg.Location[0],
		lng: cfg.Location[1] * -1, // our code has inverted longitude
	}

	//mqtt.DEBUG = log.New(os.Stdout, "[MQTT]", 0)

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.Server).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetClientID("regelwerk").
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
