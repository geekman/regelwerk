# regelwerk

**regelwerk**, which basically means a set of rules, is a simple program that receives and reacts to events over MQTT.

Inspired by the original [regelwerk](https://github.com/stapelberg/regelwerk) from @stapelberg.

Building
=========

To build regelwerk:

    GOOS=linux  go build -trimpath -ldflags="-s -w"

