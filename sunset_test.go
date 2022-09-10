package main

import (
	"testing"
	"time"
)

func makeDate(yy, mm, dd int) time.Time {
	return time.Date(yy, time.Month(mm), dd, 0, 0, 0, 0, time.Local)
}

func TestJulianDay(t *testing.T) {
	tests := []struct {
		d            time.Time
		day, century float64
	}{
		{makeDate(2020, 1, 1), 2458849.5, 0.19998631074606435},
		{makeDate(2022, 1, 1), 2459580.5, 0.22},
	}
	for _, tt := range tests {
		jd := julianDay(tt.d)
		jc := julianCentury(jd)

		dstr := tt.d.Format("2006-01-02")
		t.Logf("%s: jd %v, jc %v", dstr, jd, jc)

		if jd != tt.day {
			t.Errorf("julian day is wrong. %s, wanted %v got %v", dstr, tt.day, jd)
		}
		if jc != tt.century {
			t.Errorf("julian century is wrong. %s, wanted %v got %v", dstr, tt.century, jc)
		}
	}
}

func TestSunriseSunset(t *testing.T) {
	dates := []time.Time{
		makeDate(2020, 1, 1),
		makeDate(2022, 1, 1),
	}
	for _, d := range dates {
		rise := calcTimeAtSunAngle(d, true, 90.833, 22, -122)
		set := calcTimeAtSunAngle(d, false, 90.833, 22, -122)
		t.Logf("%v - rise %v\n", d, rise)
		t.Logf("%v - set  %v\n", d, set)
	}
}
