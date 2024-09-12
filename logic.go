package main

import (
	"log"
)

func (r *regelwerk) handleDeviceEvent(d *device, payload map[string]any) {
	switch d.id {
	case "switch":
		action := getMapValue(payload, "action")

		if action == "single_right" {
			if *debugMode {
				log.Printf("switch actuated: %v", action)
			}

			if r.DestroyTimer("contact") {
				log.Printf("manual override - discarding current session")
			}
		}
	}
}

func (r *regelwerk) handleDeviceChangedEvent(d *device, payload map[string]any) {
	switch d.id {
	case "contact":
		if d.state != true { // door opened
			// either stop the timer, or we add a timer if we should turn on
			if r.StopTimer("contact") != nil {
				log.Printf("paused session for triggered sensor")
			} else if r.LookupDevice("switch").state != "ON" && r.NowIsDusk() {
				log.Printf("starting session for triggered sensor")
				r.AddTimer("contact")

				// send turn on
				defer r.sendSwitchState(true)
			}
		} else {
			// door closed, start countdown timer if any
			if r.StartTimer("contact", r.offDelay) {
				log.Printf("starting delayed turn-off after %s", r.offDelay)
			}
		}
	}
}

func (r *regelwerk) handleTimer(name string, expired bool) {
	switch name {
	case "contact":
		// turn off lights after timeout/expiry
		r.sendSwitchState(false)
	}
}
