package main

import (
	"log"
)

func (r *regelwerk) setSwitchState(state string) {
	r.LookupDevice("switch").SendNewState(r.client, state)
}

func (r *regelwerk) handleDeviceEvent(d *device, payload map[string]any) {
	switch d.id {
	case "switch":
		action := getMapValue(payload, "action")

		if action == "single_right" {
			if *debugMode {
				log.Printf("switch actuated: %v", action)
			}

			if r.DestroyTimer("contact") || r.DestroyTimer("motion") {
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
			} else if t2 := r.StopTimer("motion"); t2 != nil ||
				(r.LookupDevice("switch").state != "ON" && r.NowIsDusk()) {

				if t2 != nil {
					log.Printf("converting motion->contact session")
					r.DestroyTimer("motion")
				} else {
					log.Printf("starting session for triggered sensor")
				}

				r.AddTimer("contact")

				// send turn on
				go r.setSwitchState("ON")
			}
		} else {
			// door closed, start countdown timer if any
			if r.StartTimer("contact", r.offDelay) {
				log.Printf("starting delayed turn-off after %s", r.offDelay)
			}
		}

	case "motion":
		if d.state == true { // motion detected
			if r.StopTimer("motion") != nil {
				log.Printf("paused session for triggered sensor")
			} else if r.LookupDevice("switch").state != "ON" && r.NowIsDusk() {
				log.Printf("starting session for triggered sensor")
				r.AddTimerWithExpiry("motion", r.motionExpiry)

				go r.setSwitchState("ON")
			}
		} else {
			// no more motion, start countdown timer if any
			if r.StartTimer("motion", r.motionOffDelay) {
				log.Printf("starting delayed turn-off after %s", r.motionOffDelay)
			}
		}
	}
}

func (r *regelwerk) handleTimer(name string, expired bool) {
	switch name {
	case "contact", "motion":
		// turn off lights after timeout/expiry
		r.setSwitchState("OFF")

		// in case of a stuck sensor, reset occupancy to false to have it
		// re-trigger immediately when next reporting
		if name == "motion" && expired {
			r.LookupDevice("motion").state = false
		}
	}
}
